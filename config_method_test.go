package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// configMethod builds the method string bridge-server actually puts on the
// wire: "config:" followed by a marshalled msg.ConfigSessionRequest, with no
// params (llm-bridge-server internal/server/sessions.go handleConfigSession).
// Building it from the canonical type rather than a hand-written literal is
// what makes these tests fail if that wire shape ever changes.
func configMethod(t *testing.T, req msg.ConfigSessionRequest) string {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal config request: %v", err)
	}
	return "config:" + string(payload)
}

// TestConfigMethodReachesHandler is the regression pin for the defect itself:
// before the prefix branch existed, every config bridge-server sent fell
// through to "unknown method" and changed nothing.
func TestConfigMethodReachesHandler(t *testing.T) {
	h := testHarness()
	// Start from a discovered model, not a pinned one: testHarness pins it, and
	// asserting a flag that was already true proves nothing.
	h.cfg.ModelExplicit = false
	getEvents := captureEvents(t)

	method := configMethod(t, msg.ConfigSessionRequest{Model: "hermes-4-large"})
	if err := h.handleRequest(request{Method: method}); err != nil {
		t.Fatalf("handleRequest(%q): %v", method, err)
	}

	if h.cfg.Model != "hermes-4-large" {
		t.Errorf("model = %q, want hermes-4-large", h.cfg.Model)
	}
	if !h.cfg.ModelExplicit {
		t.Error("ModelExplicit should be true so discovery cannot overwrite the caller's choice")
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].System == nil || events[0].System.Subtype != "config_updated" {
		t.Fatalf("expected config_updated, got %+v", events[0].System)
	}
	if !strings.Contains(events[0].System.Message, "hermes-4-large") {
		t.Errorf("message should name the applied model: %s", events[0].System.Message)
	}
}

// TestConfigMethodNoParams pins the shape as well as the routing: the server
// sends the payload in the METHOD string and leaves params empty, so a handler
// that reads req.Params would see nothing.
func TestConfigMethodNoParams(t *testing.T) {
	h := testHarness()
	captureEvents(t)

	req := request{Method: configMethod(t, msg.ConfigSessionRequest{Model: "m1"})}
	if len(req.Params) != 0 {
		t.Fatalf("params should be empty on the config path, got %s", req.Params)
	}
	if err := h.handleRequest(req); err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if h.cfg.Model != "m1" {
		t.Errorf("model = %q, want m1", h.cfg.Model)
	}
}

// TestConfigTakesEffectOnTheNextTurn is the reason the Model control is earned
// rather than merely acknowledged: the turn hermes builds after a config names
// the new model.
func TestConfigTakesEffectOnTheNextTurn(t *testing.T) {
	h := testHarness()
	captureEvents(t)

	if err := h.handleRequest(request{Method: configMethod(t, msg.ConfigSessionRequest{Model: "next-turn-model"})}); err != nil {
		t.Fatalf("handleRequest: %v", err)
	}

	// sendMessageDirect reads h.cfg.Model when it builds responsesRequest.
	req := responsesRequest{Model: h.cfg.Model}
	if req.Model != "next-turn-model" {
		t.Errorf("next turn would be issued against %q, want next-turn-model", req.Model)
	}
}

func TestConfigUnsupportedFieldsAreNamedBack(t *testing.T) {
	budget := 12.5
	cases := []struct {
		name    string
		request msg.ConfigSessionRequest
		want    string
	}{
		{"effort", msg.ConfigSessionRequest{Effort: "high"}, "effort"},
		{"max_budget", msg.ConfigSessionRequest{MaxBudget: &budget}, "max_budget"},
		{"disabled_tools", msg.ConfigSessionRequest{DisabledTools: []string{"bash"}}, "disabled_tools"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testHarness()
			getEvents := captureEvents(t)

			err := h.handleRequest(request{Method: configMethod(t, tc.request)})
			if err == nil {
				t.Fatalf("hermes has no %s setting; the caller must be told, not left to assume it applied", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name the refused field: %v", err)
			}

			events := getEvents()
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			if events[0].System == nil || events[0].System.Subtype != "config_ignored" {
				t.Fatalf("expected config_ignored, got %+v", events[0].System)
			}
			if !strings.Contains(events[0].System.Message, tc.want) {
				t.Errorf("message should name the refused field: %s", events[0].System.Message)
			}
		})
	}
}

// A config that hermes can only half-honour applies the half it has and still
// names the other, rather than reporting a clean success.
func TestConfigAppliesModelAndStillNamesTheRefusedField(t *testing.T) {
	h := testHarness()
	getEvents := captureEvents(t)

	err := h.handleRequest(request{Method: configMethod(t, msg.ConfigSessionRequest{
		Model:  "half-honoured",
		Effort: "high",
	})})
	if err == nil {
		t.Fatal("expected the refused effort field to surface as an error")
	}
	if h.cfg.Model != "half-honoured" {
		t.Errorf("model = %q, want half-honoured — the supported half must still apply", h.cfg.Model)
	}

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	msgText := events[0].System.Message
	if events[0].System.Subtype != "config_updated" {
		t.Errorf("subtype = %q, want config_updated", events[0].System.Subtype)
	}
	if !strings.Contains(msgText, "half-honoured") || !strings.Contains(msgText, "effort") {
		t.Errorf("message should name both the applied and the refused field: %s", msgText)
	}
}

func TestConfigRejectsEmptyAndUnparseablePayloads(t *testing.T) {
	cases := []struct {
		name   string
		method string
		want   string
	}{
		{"empty payload", "config:", "empty payload"},
		{"unparseable payload", "config:{not json}", "parse config payload"},
		{"payload sets nothing", "config:{}", "sets nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testHarness()
			captureEvents(t)
			err := h.handleRequest(request{Method: tc.method})
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// set_model and a config payload are two doors onto one change; neither may
// leave the harness in a state the other would not have produced.
func TestSetModelAndConfigAgreeOnWhatChangingAModelMeans(t *testing.T) {
	viaSetModel := testHarness()
	viaConfig := testHarness()
	viaSetModel.cfg.ModelExplicit = false
	viaConfig.cfg.ModelExplicit = false
	captureEvents(t)

	if err := viaSetModel.handleRequest(request{
		Method: "set_model",
		Params: json.RawMessage(`{"model":"agreed-model"}`),
	}); err != nil {
		t.Fatalf("set_model: %v", err)
	}
	if err := viaConfig.handleRequest(request{
		Method: configMethod(t, msg.ConfigSessionRequest{Model: "agreed-model"}),
	}); err != nil {
		t.Fatalf("config: %v", err)
	}

	if viaSetModel.cfg.Model != viaConfig.cfg.Model {
		t.Errorf("model: set_model=%q config=%q", viaSetModel.cfg.Model, viaConfig.cfg.Model)
	}
	if viaSetModel.cfg.ModelExplicit != viaConfig.cfg.ModelExplicit {
		t.Errorf("ModelExplicit: set_model=%v config=%v",
			viaSetModel.cfg.ModelExplicit, viaConfig.cfg.ModelExplicit)
	}
}

// TestSIGINTCancelsTheTurnAndLeavesTheHarnessAlive pins the interrupt contract:
// bridge-server's Stop is a SIGINT, after which it marks the session idle and
// keeps the process registered. Exiting here used to end the session.
func TestSIGINTCancelsTheTurnAndLeavesTheHarnessAlive(t *testing.T) {
	h := testHarness()
	cancelled := make(chan struct{}, 4)
	h.turnMu.Lock()
	h.turnCancel = func() { cancelled <- struct{}{} }
	h.turnMu.Unlock()

	captureEvents(t)

	terminated := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		watchSignals(h, sigs, func() { terminated <- struct{}{} })
		close(done)
	}()

	sigs <- syscall.SIGINT
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not cancel the in-flight turn")
	}

	// A second Stop in the same session must also be honoured. A single
	// `sig := <-sigs` read leaves this one unread in the channel forever.
	h.turnMu.Lock()
	h.turnCancel = func() { cancelled <- struct{}{} }
	h.turnMu.Unlock()
	sigs <- syscall.SIGINT
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("a second SIGINT was dropped; the handler reads the channel only once")
	}

	select {
	case <-terminated:
		t.Fatal("SIGINT terminated the harness; it must only cancel the turn")
	case <-done:
		t.Fatal("the signal loop stopped watching after a SIGINT")
	default:
	}

	// SIGTERM still means shut down.
	sigs <- syscall.SIGTERM
	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not shut the harness down")
	}
	<-done
}

// TestSIGINTIsDeliveredToTheHandler closes the last gap the channel-level test
// leaves open: that the process is actually subscribed to SIGINT. It sends a
// real signal to this test process, which signal.Notify captures rather than
// letting it kill the run.
func TestSIGINTIsDeliveredToTheHandler(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	defer signal.Stop(sigs)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill self with SIGINT: %v", err)
	}
	select {
	case got := <-sigs:
		if got != syscall.SIGINT {
			t.Errorf("received %v, want SIGINT", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT was never delivered")
	}
}
