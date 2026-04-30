package main

import (
	"bytes"
	"encoding/json"
	"os"
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

func TestHandleStart_SetsState(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	// Start with no prompt — should go running then immediately idle.
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

	events := getEvents()
	// Should emit: running state, then idle state (no prompt = no message sent)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}
	if events[0].Type != msg.EventSessionState || events[0].State == nil || events[0].State.State != msg.SessionRunning {
		t.Errorf("first event should be running state, got %+v", events[0])
	}
	if events[1].Type != msg.EventSessionState || events[1].State == nil || events[1].State.State != msg.SessionIdle {
		t.Errorf("second event should be idle state, got %+v", events[1])
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
	e := makeEvent("sess", msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "test"}
	})
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
			emitEvent(makeEvent("sess", msg.EventSystem, nil, func(e *msg.Event) {
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
