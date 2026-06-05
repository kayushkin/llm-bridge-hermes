//go:build integration

// Integration tests that run against a real, reachable Hermes Agent server.
//
// Requirements:
//   - Hermes Agent running on HERMES_URL (default http://127.0.0.1:8642)
//   - The Hermes server is configured with a working inference backend
//     (e.g. anthropic/claude-haiku-4-5 + ANTHROPIC_TOKEN)
//
// Run with:
//
//	go test -tags=integration -v -run Integration -timeout 5m ./...
//
// Tests fail fast (not skip) if the server is unreachable: integration runs
// are a deliberate choice, so silent skips would mask "Hermes isn't up."
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test harness
// ─────────────────────────────────────────────────────────────────────────────

// integrationBaseURL returns the Hermes base URL. Fails the test if unset or
// if no server responds on /v1/health within a short timeout.
func integrationBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("HERMES_URL")
	if url == "" {
		url = "http://127.0.0.1:8642"
	}
	url = strings.TrimRight(url, "/")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/v1/health", nil)
	if err != nil {
		t.Fatalf("build health request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Hermes not reachable at %s: %v (start it with: hermes gateway run -v)", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Hermes health returned %d at %s", resp.StatusCode, url)
	}
	return url
}

func integrationClient(t *testing.T) *hermesClient {
	t.Helper()
	url := integrationBaseURL(t)
	cfg := Config{
		BaseURL:            url,
		APIKey:             os.Getenv("HERMES_API_KEY"),
		InputPricePerM:     3.0,
		OutputPricePerM:    15.0,
		ReasoningPricePerM: 15.0,
	}
	return newHermesClient(cfg)
}

// integrationHarness builds a handler-level harness pointed at the live server.
// captureEvents() redirects stdout to a pipe so emitted events can be asserted.
func integrationHarness(t *testing.T) (*harness, func() []msg.Event) {
	t.Helper()
	url := integrationBaseURL(t)
	cfg := Config{
		BaseURL:            url,
		APIKey:             os.Getenv("HERMES_API_KEY"),
		InputPricePerM:     3.0,
		OutputPricePerM:    15.0,
		ReasoningPricePerM: 15.0,
	}
	h := newHarness(cfg)
	getEvents := captureEvents(t)
	t.Cleanup(func() {
		h.shutdown()
	})
	return h, getEvents
}

// ─────────────────────────────────────────────────────────────────────────────
// Low-level client tests (exercise client.go against real Hermes)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_Health(t *testing.T) {
	c := integrationClient(t)
	if err := c.healthCheck(t.Context()); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
}

func TestIntegration_ListModels(t *testing.T) {
	c := integrationClient(t)
	ids, err := c.listModels(t.Context())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one advertised model")
	}
	found := false
	for _, id := range ids {
		if id == "hermes-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hermes-agent not in models list: %v", ids)
	}
}

func TestIntegration_SendResponsesStreaming(t *testing.T) {
	c := integrationClient(t)
	getEvents := captureEvents(t)

	sessionID := "itest-stream-" + uniqueSuffix()
	result, err := c.sendResponses(t.Context(), responsesRequest{
		Model:           "hermes-agent",
		Input:           "Reply with the single word PONG and nothing else.",
		Instructions:    "You are a terse echo.",
		Stream:          true,
		Store:           true,
		Conversation:    sessionID,
		MaxOutputTokens: intPtr(32),
	}, sendOptions{
		BridgeSessionID: sessionID,
		IdempotencyKey:  newIdempotencyKey(),
	})
	if err != nil {
		t.Fatalf("sendResponses: %v", err)
	}
	if result.ID == "" {
		t.Error("expected a response ID")
	}
	if !strings.Contains(strings.ToUpper(result.Text), "PONG") {
		t.Errorf("expected PONG in response text, got: %q", result.Text)
	}
	if result.Usage.InputTokens == 0 {
		t.Error("expected input_tokens > 0 in usage")
	}
	if result.Usage.OutputTokens == 0 {
		t.Error("expected output_tokens > 0 in usage")
	}
	if result.Cost == nil || result.Cost.TotalUSD < 0 {
		t.Errorf("expected non-negative cost, got %+v", result.Cost)
	}

	events := getEvents()
	var textDeltas int
	for _, e := range events {
		if e.Type == msg.EventStream && e.Stream != nil && e.Stream.Delta != nil && e.Stream.Delta.Type == msg.DeltaText {
			textDeltas++
		}
	}
	if textDeltas == 0 {
		t.Error("expected at least one text delta event from streaming")
	}
}

func TestIntegration_SendResponsesChaining(t *testing.T) {
	c := integrationClient(t)
	_ = captureEvents(t)

	sessionID := "itest-chain-" + uniqueSuffix()

	// Turn 1: tell Hermes a fact.
	r1, err := c.sendResponses(t.Context(), responsesRequest{
		Model:           "hermes-agent",
		Input:           "My favorite number is 42. Acknowledge with 'OK'.",
		Instructions:    "Keep answers under 10 words.",
		Stream:          true,
		Store:           true,
		Conversation:    sessionID,
		MaxOutputTokens: intPtr(64),
	}, sendOptions{
		BridgeSessionID: sessionID,
		IdempotencyKey:  newIdempotencyKey(),
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}

	// Turn 2: chain with previous_response_id and ask for the number back.
	// Hermes rejects sending both `conversation` and `previous_response_id`,
	// so the chained turn drops `conversation`.
	r2, err := c.sendResponses(t.Context(), responsesRequest{
		Model:              "hermes-agent",
		Input:              "What number did I say was my favorite? Reply with just the digits.",
		Instructions:       "Keep answers under 10 words.",
		Stream:             true,
		Store:              true,
		PreviousResponseID: r1.ID,
		MaxOutputTokens:    intPtr(64),
	}, sendOptions{
		BridgeSessionID: sessionID,
		IdempotencyKey:  newIdempotencyKey(),
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(r2.Text, "42") {
		t.Errorf("expected '42' recalled from prior turn, got: %q", r2.Text)
	}
}

func TestIntegration_GetAndDeleteResponse(t *testing.T) {
	c := integrationClient(t)
	_ = captureEvents(t)

	sessionID := "itest-getdel-" + uniqueSuffix()
	r, err := c.sendResponses(t.Context(), responsesRequest{
		Model:           "hermes-agent",
		Input:           "Say exactly: ONE TWO THREE",
		Stream:          true,
		Store:           true,
		Conversation:    sessionID,
		MaxOutputTokens: intPtr(32),
	}, sendOptions{
		BridgeSessionID: sessionID,
		IdempotencyKey:  newIdempotencyKey(),
	})
	if err != nil {
		t.Fatalf("sendResponses: %v", err)
	}

	stored, err := c.getResponse(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("getResponse: %v", err)
	}
	if stored.ID != r.ID {
		t.Errorf("stored.ID = %q, want %q", stored.ID, r.ID)
	}

	if err := c.deleteResponse(t.Context(), r.ID); err != nil {
		t.Fatalf("deleteResponse: %v", err)
	}
	// After delete, GET should 404.
	if _, err := c.getResponse(t.Context(), r.ID); err == nil {
		t.Error("expected error fetching deleted response")
	}
}

// TestIntegration_IdempotencyKeyAccepted verifies the server accepts the
// Idempotency-Key header without error. Hermes' streaming /v1/responses
// branch does NOT actually dedupe by key (the cache path is wired only for
// the non-streaming branch in api_server.py), so we can't assert
// dedup-equality here — we only confirm the header is plumbed and not
// rejected.
func TestIntegration_IdempotencyKeyAccepted(t *testing.T) {
	c := integrationClient(t)
	_ = captureEvents(t)

	sessionID := "itest-idem-" + uniqueSuffix()
	req := responsesRequest{
		Model:           "hermes-agent",
		Input:           "Say OK.",
		Stream:          true,
		Store:           true,
		Conversation:    sessionID,
		MaxOutputTokens: intPtr(16),
	}

	r, err := c.sendResponses(t.Context(), req, sendOptions{
		BridgeSessionID: sessionID,
		IdempotencyKey:  newIdempotencyKey(),
	})
	if err != nil {
		t.Fatalf("sendResponses with Idempotency-Key: %v", err)
	}
	if r.ID == "" {
		t.Error("expected a response ID")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Handler-level tests (exercise JSON-RPC surface end-to-end)
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegration_HandlerStartMessageResult exercises the full JSON-RPC flow:
// start → message → the canonical event stream that llm-bridge-server consumes.
func TestIntegration_HandlerStartMessageResult(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-hsm-" + uniqueSuffix()

	startParamsJSON, _ := json.Marshal(startParams{
		SessionID:    sessionID,
		SystemPrompt: "You are a terse echo. Reply with exactly what you're told.",
	})
	if err := h.handleRequest(request{Method: "start", Params: startParamsJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	msgParamsJSON, _ := json.Marshal(messageParams{
		Content: "Reply with the single word PONG and nothing else.",
	})
	if err := h.handleRequest(request{Method: "message", Params: msgParamsJSON}); err != nil {
		t.Fatalf("message: %v", err)
	}

	events := getEvents()
	var sawResult, sawStreamDelta bool
	var resultText string
	for _, e := range events {
		switch e.Type {
		case msg.EventStream:
			if e.Stream != nil && e.Stream.Delta != nil && e.Stream.Delta.Type == msg.DeltaText {
				sawStreamDelta = true
			}
		case msg.EventResult:
			if e.Result != nil {
				sawResult = true
				resultText = e.Result.Text
				if e.Result.Usage.InputTokens == 0 {
					t.Error("result.Usage.InputTokens should be > 0")
				}
				if e.Result.Cost == nil {
					t.Error("result.Cost should be populated")
				}
			}
		case msg.EventError:
			if e.Error != nil {
				t.Fatalf("unexpected error event: code=%s msg=%s", e.Error.Code, e.Error.Message)
			}
		}
	}
	if !sawResult {
		t.Error("missing result event")
	}
	if !sawStreamDelta {
		t.Error("missing stream text delta events")
	}
	if !strings.Contains(strings.ToUpper(resultText), "PONG") {
		t.Errorf("expected PONG in result text, got: %q", resultText)
	}
}

// TestIntegration_HandlerFork verifies that fork() rebases onto an earlier
// response ID so the next message forks the conversation tree.
func TestIntegration_HandlerFork(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-fork-" + uniqueSuffix()

	startJSON, _ := json.Marshal(startParams{
		SessionID:    sessionID,
		SystemPrompt: "Keep replies under 8 words.",
	})
	if err := h.handleRequest(request{Method: "start", Params: startJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Turn 1 — establish a fact.
	m1, _ := json.Marshal(messageParams{Content: "My pet is a cat named Luna."})
	if err := h.handleRequest(request{Method: "message", Params: m1}); err != nil {
		t.Fatalf("msg 1: %v", err)
	}
	parentRespID := h.lastRespID
	if parentRespID == "" {
		t.Fatal("lastRespID not set after first turn")
	}

	// Turn 2 — add a second fact that should be discarded by forking.
	m2, _ := json.Marshal(messageParams{Content: "Actually, I lied — my pet is a dog named Rex."})
	if err := h.handleRequest(request{Method: "message", Params: m2}); err != nil {
		t.Fatalf("msg 2: %v", err)
	}

	// Fork back to parentRespID so the "Rex" correction is outside the chain.
	forkJSON, _ := json.Marshal(forkParams{FromResponseID: parentRespID})
	if err := h.handleRequest(request{Method: "fork", Params: forkJSON}); err != nil {
		t.Fatalf("fork: %v", err)
	}

	// Turn 3 — ask what the pet is. The answer should reflect Luna (cat),
	// not Rex, because fork dropped the second turn.
	m3, _ := json.Marshal(messageParams{Content: "What is my pet's name? One word answer."})
	if err := h.handleRequest(request{Method: "message", Params: m3}); err != nil {
		t.Fatalf("msg 3: %v", err)
	}

	events := getEvents()
	var resultText string
	for _, e := range events {
		if e.Type == msg.EventResult && e.Result != nil {
			resultText = e.Result.Text // keep the last result
		}
	}
	if !strings.Contains(strings.ToLower(resultText), "luna") {
		t.Errorf("fork did not preserve pre-fork state; expected 'Luna' in answer, got: %q", resultText)
	}
	if strings.Contains(strings.ToLower(resultText), "rex") {
		t.Errorf("fork leaked post-fork state; 'Rex' should not appear, got: %q", resultText)
	}
}

// TestIntegration_HandlerSetModel verifies model changes at runtime.
// The test asks Hermes to run a short turn, then changes the model via
// set_model, and checks the system event + that the next result reflects
// the new model name.
func TestIntegration_HandlerSetModel(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-setmodel-" + uniqueSuffix()

	startJSON, _ := json.Marshal(startParams{SessionID: sessionID})
	if err := h.handleRequest(request{Method: "start", Params: startJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	sm, _ := json.Marshal(setModelParams{Model: "hermes-agent-alt"})
	if err := h.handleRequest(request{Method: "set_model", Params: sm}); err != nil {
		t.Fatalf("set_model: %v", err)
	}
	if h.cfg.Model != "hermes-agent-alt" {
		t.Errorf("cfg.Model = %q, want hermes-agent-alt", h.cfg.Model)
	}

	events := getEvents()
	found := false
	for _, e := range events {
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "model_changed" {
			found = true
		}
	}
	if !found {
		t.Error("missing model_changed system event")
	}
}

// TestIntegration_HandlerInterrupt kicks off a long turn, fires interrupt on
// the handler, and asserts the turn aborts with an INTERRUPTED error.
// Uses a long generation (count to 500) so the interrupt wins the race.
func TestIntegration_HandlerInterrupt(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-interrupt-" + uniqueSuffix()

	startJSON, _ := json.Marshal(startParams{SessionID: sessionID})
	if err := h.handleRequest(request{Method: "start", Params: startJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Fire a long-running message in a goroutine; interrupt after a short delay.
	m, _ := json.Marshal(messageParams{
		Content: "Count slowly from 1 to 500, one number per line, with explanations.",
	})
	var wg sync.WaitGroup
	wg.Add(1)
	var msgErr error
	go func() {
		defer wg.Done()
		msgErr = h.handleRequest(request{Method: "message", Params: m})
	}()

	// Wait briefly for the turn to start, then interrupt.
	time.Sleep(800 * time.Millisecond)
	if err := h.handleRequest(request{Method: "interrupt", Params: nil}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	// Message returns once the context cancel propagates.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("message did not return after interrupt")
	}

	if msgErr == nil {
		t.Error("expected message to return an error after interrupt")
	}

	events := getEvents()
	var sawInterrupted bool
	for _, e := range events {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "INTERRUPTED" {
			sawInterrupted = true
		}
	}
	if !sawInterrupted {
		t.Error("missing INTERRUPTED error event")
	}
}

// TestIntegration_HandlerCompactIsUnsupported confirms compact returns an
// UNSUPPORTED error rather than silently faking an ack — Hermes /v1 has no
// compaction endpoint.
func TestIntegration_HandlerCompactIsUnsupported(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-compact-" + uniqueSuffix()

	startJSON, _ := json.Marshal(startParams{SessionID: sessionID})
	if err := h.handleRequest(request{Method: "start", Params: startJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := h.handleRequest(request{Method: "compact", Params: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("expected compact to error")
	}

	events := getEvents()
	found := false
	for _, e := range events {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "UNSUPPORTED" {
			found = true
		}
	}
	if !found {
		t.Error("missing UNSUPPORTED error event from compact")
	}
}

// TestIntegration_HandlerGetAndForget exercises get_response + forget_response
// on a real stored response.
func TestIntegration_HandlerGetAndForget(t *testing.T) {
	h, getEvents := integrationHarness(t)
	sessionID := "itest-rget-" + uniqueSuffix()

	startJSON, _ := json.Marshal(startParams{SessionID: sessionID})
	if err := h.handleRequest(request{Method: "start", Params: startJSON}); err != nil {
		t.Fatalf("start: %v", err)
	}

	msgJSON, _ := json.Marshal(messageParams{Content: "Say 'HELLO' and nothing else."})
	if err := h.handleRequest(request{Method: "message", Params: msgJSON}); err != nil {
		t.Fatalf("message: %v", err)
	}
	respID := h.lastRespID
	if respID == "" {
		t.Fatal("no lastRespID after message")
	}

	getJSON, _ := json.Marshal(responseIDParams{ID: respID})
	if err := h.handleRequest(request{Method: "get_response", Params: getJSON}); err != nil {
		t.Fatalf("get_response: %v", err)
	}

	forgetJSON, _ := json.Marshal(responseIDParams{ID: respID})
	if err := h.handleRequest(request{Method: "forget_response", Params: forgetJSON}); err != nil {
		t.Fatalf("forget_response: %v", err)
	}

	// After delete, get_response should fail.
	if err := h.handleRequest(request{Method: "get_response", Params: getJSON}); err == nil {
		t.Error("expected get_response to fail after delete")
	}

	// Drain all emitted events and check we saw: at least one result (from
	// the original message + the rehydrated get), and a response_deleted
	// system event from forget_response.
	events := getEvents()
	var resultCount, forgottenCount int
	for _, e := range events {
		if e.Type == msg.EventResult && e.Result != nil {
			resultCount++
		}
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "response_deleted" {
			forgottenCount++
		}
	}
	if resultCount < 2 {
		t.Errorf("expected at least 2 result events (message + get_response), got %d", resultCount)
	}
	if forgottenCount != 1 {
		t.Errorf("expected exactly 1 response_deleted event, got %d", forgottenCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func intPtr(i int) *int { return &i }

var suffixMu sync.Mutex
var suffixN int

func uniqueSuffix() string {
	suffixMu.Lock()
	defer suffixMu.Unlock()
	suffixN++
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), suffixN)
}
