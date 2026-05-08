package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// captureEvents redirects emitEvent output to a buffer and returns
// a function that parses and returns the captured events.
func captureEvents(t *testing.T) func() []msg.Event {
	t.Helper()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	t.Cleanup(func() { os.Stdout = origStdout })

	return func() []msg.Event {
		w.Close()
		var buf bytes.Buffer
		buf.ReadFrom(r)
		os.Stdout = origStdout

		var events []msg.Event
		for _, line := range strings.Split(buf.String(), "\n") {
			if line == "" {
				continue
			}
			var ev msg.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Errorf("unmarshal event: %v (line: %s)", err, line)
				continue
			}
			events = append(events, ev)
		}
		return events
	}
}

func testHarness() *harness {
	cfg := Config{
		BaseURL:         "http://localhost:9999",
		Model:           "test-model",
		ModelExplicit:   true,
		InputPricePerM:  3.0,
		OutputPricePerM: 15.0,
	}
	h := newHarness(cfg)
	h.sessionID = "test-session"
	h.conversation = "test-session"
	return h
}

func TestHandleRequest_UnknownMethod(t *testing.T) {
	h := testHarness()
	err := h.handleRequest(request{Method: "bogus", Params: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("expected unknown method error, got: %v", err)
	}
}

func TestHandleRequest_InvalidJSON(t *testing.T) {
	h := testHarness()

	methods := []string{"start", "message", "set_model", "fork", "get_response", "forget_response"}
	for _, m := range methods {
		err := h.handleRequest(request{Method: m, Params: json.RawMessage(`{invalid}`)})
		if err == nil {
			t.Errorf("method %s: expected parse error for invalid JSON", m)
		}
	}
}

func TestHandleCompact(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	err := h.handleCompact(compactParams{})
	if err == nil {
		t.Fatal("expected error from handleCompact")
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != msg.EventError {
		t.Errorf("event type = %s, want error", events[0].Type)
	}
	if events[0].Error == nil || events[0].Error.Code != "UNSUPPORTED" {
		t.Errorf("error code = %v, want UNSUPPORTED", events[0].Error)
	}
}

func TestHandleResume(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	err := h.handleResume()
	if err != nil {
		t.Fatalf("handleResume: %v", err)
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != msg.EventSystem {
		t.Errorf("event type = %s, want system", events[0].Type)
	}
	if events[0].System == nil || events[0].System.Subtype != "resume" {
		t.Errorf("subtype = %v, want resume", events[0].System)
	}
}

func TestHandleInterrupt_NoTurn(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	err := h.handleInterrupt()
	if err != nil {
		t.Fatalf("handleInterrupt: %v", err)
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].System == nil || events[0].System.Subtype != "interrupt_noop" {
		t.Errorf("expected interrupt_noop, got %+v", events[0].System)
	}
}

func TestHandleInterrupt_ActiveTurn(t *testing.T) {
	h := testHarness()

	// Simulate an active turn by setting turnCancel.
	cancelled := false
	h.turnMu.Lock()
	h.turnCancel = func() { cancelled = true }
	h.turnMu.Unlock()

	getEvents := captureEvents(t)
	err := h.handleInterrupt()
	if err != nil {
		t.Fatalf("handleInterrupt: %v", err)
	}
	if !cancelled {
		t.Error("expected turn context to be cancelled")
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].System == nil || events[0].System.Subtype != "interrupt" {
		t.Errorf("expected interrupt subtype, got %+v", events[0].System)
	}
}

func TestHandleSetModel(t *testing.T) {
	h := testHarness()

	t.Run("empty model fails", func(t *testing.T) {
		err := h.handleSetModel(setModelParams{Model: ""})
		if err == nil {
			t.Error("expected error for empty model")
		}
	})

	t.Run("sets model", func(t *testing.T) {
		getEvents := captureEvents(t)
		err := h.handleSetModel(setModelParams{Model: "new-model"})
		if err != nil {
			t.Fatalf("handleSetModel: %v", err)
		}
		if h.cfg.Model != "new-model" {
			t.Errorf("model = %q, want new-model", h.cfg.Model)
		}
		if !h.cfg.ModelExplicit {
			t.Error("ModelExplicit should be true after set_model")
		}

		events := getEvents()
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0].System == nil || events[0].System.Subtype != "model_changed" {
			t.Errorf("expected model_changed, got %+v", events[0].System)
		}
		if !strings.Contains(events[0].System.Message, "new-model") {
			t.Errorf("message should mention new model: %s", events[0].System.Message)
		}
	})
}

func TestHandleFork(t *testing.T) {
	h := testHarness()
	h.lastRespID = "resp-1"

	t.Run("empty from_response_id fails", func(t *testing.T) {
		err := h.handleFork(forkParams{})
		if err == nil {
			t.Error("expected error for empty from_response_id")
		}
	})

	t.Run("sets lastRespID and conversation", func(t *testing.T) {
		getEvents := captureEvents(t)
		err := h.handleFork(forkParams{
			FromResponseID: "resp-0",
			Conversation:   "fork-branch",
		})
		if err != nil {
			t.Fatalf("handleFork: %v", err)
		}
		if h.lastRespID != "resp-0" {
			t.Errorf("lastRespID = %q, want resp-0", h.lastRespID)
		}
		if h.conversation != "fork-branch" {
			t.Errorf("conversation = %q, want fork-branch", h.conversation)
		}
		events := getEvents()
		if len(events) != 1 || events[0].System == nil || events[0].System.Subtype != "fork" {
			t.Errorf("expected fork event, got %+v", events)
		}
	})

	t.Run("preserves conversation when not provided", func(t *testing.T) {
		h.conversation = "original"
		_ = captureEvents(t)
		_ = h.handleFork(forkParams{FromResponseID: "resp-2"})
		if h.conversation != "original" {
			t.Errorf("conversation changed to %q, should stay original", h.conversation)
		}
	})
}

func TestHandleRetry_NoPriorInput(t *testing.T) {
	h := testHarness()
	err := h.handleRetry()
	if err == nil || !strings.Contains(err.Error(), "no prior input") {
		t.Errorf("expected no prior input error, got: %v", err)
	}
}

func TestHandleGetResponse_EmptyID(t *testing.T) {
	h := testHarness()
	err := h.handleGetResponse(responseIDParams{})
	if err == nil {
		t.Error("expected error for empty id")
	}
}

func TestHandleForgetResponse_EmptyID(t *testing.T) {
	h := testHarness()
	err := h.handleForgetResponse(responseIDParams{})
	if err == nil {
		t.Error("expected error for empty id")
	}
}

// TestHandleStart_Resume verifies the canonical resume path: start{Resume:true}
// with an explicit HarnessSessionID restores the prior Hermes conversation name
// (rather than minting a fresh chain from BridgeSessionID), and the next
// /v1/responses request carries that conversation through.
func TestHandleStart_Resume(t *testing.T) {
	var capturedConversation string
	var capturedPrevRespID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req responsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		capturedConversation = req.Conversation
		capturedPrevRespID = req.PreviousResponseID

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r-resume\",\"status\":\"in_progress\"}}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-resume\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	cfg := Config{
		BaseURL:         server.URL,
		APIKey:          "test-key",
		Model:           "test-model",
		ModelExplicit:   true,
		InputPricePerM:  3.0,
		OutputPricePerM: 15.0,
	}
	h := newHarness(cfg)
	getEvents := captureEvents(t)

	if err := h.handleStart(startParams{
		BridgeSessionID:  "bs_xyz",
		HarnessSessionID: "cv_abc",
		Resume:           true,
	}); err != nil {
		t.Fatalf("handleStart (resume): %v", err)
	}

	if h.bridgeSessionID != "bs_xyz" {
		t.Errorf("bridgeSessionID = %q, want bs_xyz", h.bridgeSessionID)
	}
	if h.sessionID != "cv_abc" {
		t.Errorf("sessionID = %q, want cv_abc", h.sessionID)
	}
	if h.conversation != "cv_abc" {
		t.Errorf("conversation = %q, want cv_abc (resume should restore HarnessSessionID)", h.conversation)
	}

	if err := h.sendMessageDirect("hello", "idem-resume-1"); err != nil {
		t.Fatalf("sendMessageDirect: %v", err)
	}
	if capturedConversation != "cv_abc" {
		t.Errorf("captured conversation = %q, want cv_abc", capturedConversation)
	}
	if capturedPrevRespID != "" {
		t.Errorf("captured previous_response_id = %q, want empty (first turn after resume)", capturedPrevRespID)
	}
	_ = getEvents()
}

// TestHandleStart_ColdStart verifies that cold start (Resume:false) ignores any
// HarnessSessionID present in start params and uses BridgeSessionID as the
// Hermes conversation name — preserving prior behavior.
func TestHandleStart_ColdStart(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	if err := h.handleStart(startParams{
		BridgeSessionID:  "bs_xyz",
		HarnessSessionID: "cv_abc",
		Resume:           false,
		Model:            "test-model",
	}); err != nil {
		t.Fatalf("handleStart (cold): %v", err)
	}

	if h.bridgeSessionID != "bs_xyz" {
		t.Errorf("bridgeSessionID = %q, want bs_xyz", h.bridgeSessionID)
	}
	if h.sessionID != "cv_abc" {
		t.Errorf("sessionID = %q, want cv_abc", h.sessionID)
	}
	if h.conversation != "bs_xyz" {
		t.Errorf("conversation = %q, want bs_xyz (cold start uses bridgeID)", h.conversation)
	}
	_ = getEvents()
}

// TestHandleStart_ColdStart_WireProtocol exercises the full cold-start chain
// against an httptest fake of Hermes: handleStart{BridgeSessionID:"bs_1"} with
// no HarnessSessionID, then a turn. Asserts that every emitted event stamps
// both BridgeSessionID and HarnessSessionID equal to "bs_1" (no harness-side
// rotation in Hermes), and that the resulting POST /v1/responses carries
// `conversation: bs_1` with no previous_response_id (first turn after cold
// start). Companion to TestHandleStart_Resume.
func TestHandleStart_ColdStart_WireProtocol(t *testing.T) {
	var capturedConversation string
	var capturedPrevRespID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req responsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		capturedConversation = req.Conversation
		capturedPrevRespID = req.PreviousResponseID

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r-cold\",\"status\":\"in_progress\"}}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-cold\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	cfg := Config{
		BaseURL:         server.URL,
		APIKey:          "test-key",
		Model:           "test-model",
		ModelExplicit:   true,
		InputPricePerM:  3.0,
		OutputPricePerM: 15.0,
	}
	h := newHarness(cfg)
	getEvents := captureEvents(t)

	if err := h.handleStart(startParams{
		BridgeSessionID: "bs_1",
		Resume:          false,
	}); err != nil {
		t.Fatalf("handleStart (cold): %v", err)
	}

	if h.bridgeSessionID != "bs_1" {
		t.Errorf("bridgeSessionID = %q, want bs_1", h.bridgeSessionID)
	}
	if h.sessionID != "bs_1" {
		t.Errorf("sessionID = %q, want bs_1 (no HarnessSessionID set, falls through to bridgeID)", h.sessionID)
	}
	if h.conversation != "bs_1" {
		t.Errorf("conversation = %q, want bs_1 (cold start uses bridgeID)", h.conversation)
	}

	if err := h.sendMessageDirect("hello", "idem-cold-1"); err != nil {
		t.Fatalf("sendMessageDirect: %v", err)
	}
	if capturedConversation != "bs_1" {
		t.Errorf("captured conversation = %q, want bs_1", capturedConversation)
	}
	if capturedPrevRespID != "" {
		t.Errorf("captured previous_response_id = %q, want empty (first turn after cold start)", capturedPrevRespID)
	}

	// Every emitted event must stamp both ids equal to bs_1.
	events := getEvents()
	if len(events) == 0 {
		t.Fatal("no events captured")
	}
	for i, ev := range events {
		if ev.BridgeSessionID != "bs_1" {
			t.Errorf("event[%d] BridgeSessionID = %q, want bs_1 (type=%s)", i, ev.BridgeSessionID, ev.Type)
		}
		if ev.HarnessSessionID != "bs_1" {
			t.Errorf("event[%d] HarnessSessionID = %q, want bs_1 (type=%s)", i, ev.HarnessSessionID, ev.Type)
		}
	}
}

// TestHandleStart_ForkRejected verifies the fork-from-bridge-server path
// returns FORK_UNSUPPORTED. Hermes fork is per-response (`previous_response_id`),
// not per-session — silently producing a fresh chain would violate the contract,
// so we reject the call rather than fake a fork.
func TestHandleStart_ForkRejected(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	err := h.handleStart(startParams{
		BridgeSessionID: "bs_fork",
		Fork:            "cv_parent",
	})
	if err == nil {
		t.Fatal("expected error from handleStart with Fork set")
	}
	if !strings.Contains(err.Error(), "fork unsupported") {
		t.Errorf("error = %v, want 'fork unsupported'", err)
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != msg.EventError {
		t.Errorf("event type = %s, want error", events[0].Type)
	}
	if events[0].Error == nil || events[0].Error.Code != "FORK_UNSUPPORTED" {
		t.Errorf("error code = %v, want FORK_UNSUPPORTED", events[0].Error)
	}
	if events[0].Error.Retryable {
		t.Error("FORK_UNSUPPORTED should not be retryable")
	}
	if events[0].BridgeSessionID != "bs_fork" {
		t.Errorf("event BridgeSessionID = %q, want bs_fork", events[0].BridgeSessionID)
	}
}

func TestHandleStart_StoresParams(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	// Start with no prompt — params are stored, no message is sent.
	// SessionState transitions are derived centrally by llm-bridge-server.
	err := h.handleStart(startParams{
		SessionID:    "sess-1",
		SystemPrompt: "You are helpful",
		Model:        "custom-model",
	})
	if err != nil {
		t.Fatalf("handleStart: %v", err)
	}

	if h.sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", h.sessionID)
	}
	if h.systemPrompt != "You are helpful" {
		t.Errorf("systemPrompt = %q, want 'You are helpful'", h.systemPrompt)
	}
	if h.cfg.Model != "custom-model" {
		t.Errorf("model = %q, want custom-model", h.cfg.Model)
	}

	for _, e := range getEvents() {
		if e.Type == msg.EventError {
			t.Errorf("unexpected error event: %+v", e.Error)
		}
	}
}

// TestHermes_NoStateDBLeak guards the "Hermes harness needs no state.db"
// invariant from the session-chain port. Cold-starting and shutting down
// without ever issuing a turn must leave no SQLite artifacts under any
// llm-bridge-hermes data directory — Hermes holds conversation state
// server-side, so the harness has nothing to persist.
//
// The test isolates HOME and XDG_DATA_HOME to a tempdir, runs handleStart
// (no prompt, so no /v1/responses call), shuts the harness down, then walks
// the tempdir asserting nothing matching state.db / *.sqlite was created.
// A future commit that introduces SQLite-backed state will trip this guard.
func TestHermes_NoStateDBLeak(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tempHome, ".local", "share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))

	h := testHarness()
	getEvents := captureEvents(t)

	if err := h.handleStart(startParams{
		BridgeSessionID: "bs_crash",
	}); err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	// Crash before any turn: tear down without sending a message.
	h.shutdown()
	_ = getEvents()

	// Walk the isolated tempdir; fail on any file that looks like SQLite state.
	err := filepath.Walk(tempHome, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		lower := strings.ToLower(base)
		if strings.HasSuffix(lower, ".db") ||
			strings.HasSuffix(lower, ".sqlite") ||
			strings.HasSuffix(lower, ".sqlite3") ||
			lower == "state.db" ||
			strings.HasSuffix(lower, "-wal") ||
			strings.HasSuffix(lower, "-shm") {
			t.Errorf("hermes harness leaked SQLite artifact at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tempdir: %v", err)
	}
}

func TestNewIdempotencyKey(t *testing.T) {
	// Should produce unique keys.
	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		k := newIdempotencyKey()
		if keys[k] {
			t.Fatalf("duplicate key: %s", k)
		}
		keys[k] = true
	}

	// Should be hex-encoded UUID4 (32 hex chars).
	k := newIdempotencyKey()
	if len(k) != 32 {
		t.Errorf("key length = %d, want 32", len(k))
	}
}

func TestMakeEvent(t *testing.T) {
	e := makeEvent("bs", "sess", msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "test"}
	})
	if e.BridgeSessionID != "bs" {
		t.Errorf("BridgeSessionID = %q, want bs", e.BridgeSessionID)
	}
	if e.HarnessSessionID != "sess" {
		t.Errorf("HarnessSessionID = %q, want sess", e.HarnessSessionID)
	}
	if e.Harness != msg.HarnessHermes {
		t.Errorf("Harness = %q, want hermes", e.Harness)
	}
	if e.Type != msg.EventSystem {
		t.Errorf("Type = %s, want system", e.Type)
	}
	if e.System == nil || e.System.Subtype != "test" {
		t.Errorf("System = %+v, want subtype=test", e.System)
	}
	if e.Timestamp.IsZero() {
		t.Error("Timestamp should be set")
	}
}

func TestEmitEventConcurrency(t *testing.T) {
	getEvents := captureEvents(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			emitEvent(makeEvent("bs", "sess", msg.EventSystem, nil, func(e *msg.Event) {
				e.System = &msg.SystemEvent{Subtype: "concurrent"}
			}))
		}()
	}
	wg.Wait()

	events := getEvents()
	if len(events) != 50 {
		t.Errorf("expected 50 events, got %d (potential race)", len(events))
	}
}
