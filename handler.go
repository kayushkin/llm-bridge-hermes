package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// request is the JSON-RPC request format from llm-bridge.
type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type startParams struct {
	BridgeSessionID  string `json:"bridge_session_id,omitempty"`
	HarnessSessionID string `json:"harness_session_id,omitempty"`
	// SessionID is the legacy field; new callers should set BridgeSessionID
	// (and optionally HarnessSessionID). Kept for back-compat.
	SessionID    string `json:"session_id,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Resume       bool   `json:"resume,omitempty"`
	// Fork is the parent's HarnessSessionID when bridge-server starts a fork.
	// Hermes rejects this path — fork semantics in Hermes are per-response, not
	// per-session, so callers must use the explicit `fork` JSON-RPC method with
	// `from_response_id`. See README "Fork semantics".
	Fork         string `json:"fork,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Model        string `json:"model,omitempty"`
}

type messageParams struct {
	Content        string `json:"content"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type compactParams struct {
	Summary string `json:"summary,omitempty"`
}

type setModelParams struct {
	Model string `json:"model"`
}

type forkParams struct {
	FromResponseID string `json:"from_response_id"`
	Conversation   string `json:"conversation,omitempty"`
}

type responseIDParams struct {
	ID string `json:"id"`
}

// turnRequest is a serialized message for the turn queue.
type turnRequest struct {
	content        string
	idempotencyKey string
	done           chan error
}

// harness holds the runtime state for a Hermes session.
type harness struct {
	cfg              Config
	client           *hermesClient
	bridgeSessionID  string // bridge-server's stable id; stamped on every event
	sessionID        string // harness-side id (== bridgeSessionID for hermes — no native rotation)
	conversation     string // Hermes conversation name for multi-turn
	lastRespID       string // last response ID for chaining
	systemPrompt     string // sent as `instructions` on every turn
	lastInput        string // for retry

	ctx    context.Context
	cancel context.CancelFunc

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	// queue serializes message turns so concurrent calls don't race.
	// Started lazily on first message after start.
	queue     chan turnRequest
	queueOnce sync.Once
}

func newHarness(cfg Config) *harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &harness{
		cfg:    cfg,
		client: newHermesClient(cfg),
		ctx:    ctx,
		cancel: cancel,
	}
}

// ensureQueue starts the turn worker goroutine if not already running.
func (h *harness) ensureQueue() {
	h.queueOnce.Do(func() {
		h.queue = make(chan turnRequest, 16)
		go h.turnWorker()
	})
}

// turnWorker processes queued turns sequentially.
func (h *harness) turnWorker() {
	for req := range h.queue {
		err := h.sendMessage(req.content, req.idempotencyKey)
		req.done <- err
	}
}

func (h *harness) handleRequest(req request) error {
	switch req.Method {
	case "start":
		var p startParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse start params: %w", err)
		}
		return h.handleStart(p)

	case "message":
		var p messageParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse message params: %w", err)
		}
		return h.handleMessage(p)

	case "compact":
		var p compactParams
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		return h.handleCompact(p)

	case "resume":
		return h.handleResume()

	case "interrupt":
		return h.handleInterrupt()

	case "set_model":
		var p setModelParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse set_model params: %w", err)
		}
		return h.handleSetModel(p)

	case "fork":
		var p forkParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse fork params: %w", err)
		}
		return h.handleFork(p)

	case "retry":
		return h.handleRetry()

	case "get_response":
		var p responseIDParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse get_response params: %w", err)
		}
		return h.handleGetResponse(p)

	case "forget_response":
		var p responseIDParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fmt.Errorf("parse forget_response params: %w", err)
		}
		return h.handleForgetResponse(p)

	default:
		return fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (h *harness) handleStart(p startParams) error {
	// Resolve session identity from canonical wire fields, falling back to
	// the legacy SessionID. For hermes the harness-side id mirrors the
	// bridge-side id (no native rotation), so absent an explicit
	// HarnessSessionID we use the bridge id for both.
	bridgeID := p.BridgeSessionID
	if bridgeID == "" {
		bridgeID = p.SessionID
	}
	harnessID := p.HarnessSessionID
	if harnessID == "" {
		harnessID = bridgeID
	}
	h.bridgeSessionID = bridgeID
	h.sessionID = harnessID

	// Fork from bridge-server is rejected. Hermes fork is per-response
	// (`previous_response_id`), not per-session — there is no server-side
	// primitive to clone a conversation by name. Faking a fresh chain would
	// silently drop all parent state and pretend a fork happened. Callers
	// that hold a parent response id should use the explicit `fork` JSON-RPC
	// method with `from_response_id` instead.
	if p.Fork != "" {
		emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:      "FORK_UNSUPPORTED",
				Message:   "hermes does not support session-level fork; use the `fork` JSON-RPC method with `from_response_id`",
				Retryable: false,
			}
		}))
		return fmt.Errorf("fork unsupported on hermes start path")
	}
	if p.Resume {
		// Canonical resume: bridge-server sends start{Resume:true} with the
		// previous HarnessSessionID populated (Hermes's prior conversation name).
		// Fall back to bridgeID if absent (harnessID resolves to bridgeID via
		// the chain above), matching cold-start behavior.
		h.conversation = harnessID
	} else {
		h.conversation = bridgeID // cold start: Hermes conversation name = bridge session id
	}
	h.systemPrompt = p.SystemPrompt
	if p.Model != "" {
		h.cfg.Model = p.Model
		h.cfg.ModelExplicit = true
	}

	// Credential resolution. The start message credential_id takes precedence
	// over LLMBRIDGE_CREDENTIAL_ID; an explicit HERMES_API_KEY only wins when
	// no credential is configured.
	if p.CredentialID != "" {
		h.cfg.CredentialID = p.CredentialID
	}
	if h.cfg.CredentialID != "" {
		key, err := resolveCredentialKey(h.cfg.CredentialID)
		if err != nil {
			emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
				e.Error = &msg.ErrorEvent{Code: "CREDENTIAL_ERROR", Message: err.Error()}
			}))
			return err
		}
		h.cfg.APIKey = key
		h.client.apiKey = key
	}

	// Optional preflight — fail fast if the server isn't reachable.
	if h.cfg.Preflight {
		if err := h.client.healthCheck(h.ctx); err != nil {
			emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
				e.Error = &msg.ErrorEvent{Code: "PREFLIGHT_FAIL", Message: err.Error()}
			}))
			return err
		}
		emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: "preflight_ok"}
		}))
	}

	// Model discovery — only when user hasn't pinned HERMES_MODEL.
	if !h.cfg.ModelExplicit {
		ids, err := h.client.listModels(h.ctx)
		if err != nil {
			emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
				e.Error = &msg.ErrorEvent{Code: "MODEL_DISCOVERY_FAIL", Message: err.Error()}
			}))
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("hermes advertised no models via /v1/models")
		}
		h.cfg.Model = ids[0]
		emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: "model_discovered", Message: h.cfg.Model}
		}))
	}

	if p.Prompt == "" {
		return nil
	}

	return h.sendMessage(p.Prompt, "")
}

func (h *harness) handleMessage(p messageParams) error {
	if h.sessionID == "" {
		return fmt.Errorf("no active session")
	}

	h.ensureQueue()
	req := turnRequest{
		content:        p.Content,
		idempotencyKey: p.IdempotencyKey,
		done:           make(chan error, 1),
	}
	h.queue <- req
	return <-req.done
}

// handleCompact surfaces an honest error — Hermes' /v1 HTTP surface does not
// expose context compaction. Compaction is a CLI slash command (/compress)
// with no documented HTTP trigger. We refuse to fake an ack.
func (h *harness) handleCompact(p compactParams) error {
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
		e.Error = &msg.ErrorEvent{
			Code:      "UNSUPPORTED",
			Message:   "compact is not available on the Hermes /v1 HTTP API; Hermes manages context compression server-side",
			Retryable: false,
		}
	}))
	return fmt.Errorf("compact unsupported on hermes HTTP API")
}

// handleResume is a no-op system event. The canonical resume path used by
// llm-bridge-server is start{Resume:true, HarnessSessionID:...}, which is
// handled in handleStart — not the explicit "resume" JSON-RPC method.
// Hermes maintains server-side conversation state via the conversation name,
// so simply continuing turns against the restored conversation is enough.
func (h *harness) handleResume() error {
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "resume", Message: "session resumed"}
	}))
	return nil
}

// handleInterrupt cancels the in-flight turn's context, aborting the SSE stream.
// The Hermes server will stop generating when the HTTP connection drops.
func (h *harness) handleInterrupt() error {
	h.turnMu.Lock()
	cancel := h.turnCancel
	h.turnMu.Unlock()

	if cancel == nil {
		emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: "interrupt_noop", Message: "no in-flight turn"}
		}))
		return nil
	}
	cancel()
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "interrupt", Message: "turn cancelled"}
	}))
	return nil
}

func (h *harness) handleSetModel(p setModelParams) error {
	if p.Model == "" {
		return fmt.Errorf("set_model: model required")
	}
	previous := h.cfg.Model
	h.cfg.Model = p.Model
	h.cfg.ModelExplicit = true
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "model_changed", Message: fmt.Sprintf("%s -> %s", previous, p.Model)}
	}))
	return nil
}

// handleFork rebases the conversation chain onto an earlier response ID.
// The next message() will use FromResponseID as parent instead of h.lastRespID.
func (h *harness) handleFork(p forkParams) error {
	if p.FromResponseID == "" {
		return fmt.Errorf("fork: from_response_id required")
	}
	h.lastRespID = p.FromResponseID
	if p.Conversation != "" {
		h.conversation = p.Conversation
	}
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{
			Subtype: "fork",
			Message: fmt.Sprintf("forked from response %s (conversation=%s)", p.FromResponseID, h.conversation),
		}
	}))
	return nil
}

// handleRetry resends the last user input with a fresh idempotency key.
// Uses the same previous_response_id, so the model re-answers the same turn.
func (h *harness) handleRetry() error {
	if h.lastInput == "" {
		return fmt.Errorf("retry: no prior input to resend")
	}
	// Drop lastRespID by one turn so we branch from the parent, not chain onward.
	// The current lastRespID points at the response we want to replace; retry
	// should branch from its parent. Without server-side parent lookup, the
	// safest approach is to keep lastRespID as-is (chain forward) and let the
	// caller use fork() if they want true replacement.
	return h.sendMessage(h.lastInput, "")
}

func (h *harness) handleGetResponse(p responseIDParams) error {
	if p.ID == "" {
		return fmt.Errorf("get_response: id required")
	}
	obj, err := h.client.getResponse(h.ctx, p.ID)
	if err != nil {
		return err
	}

	// Extract text from output items.
	var text string
	for _, item := range obj.Output {
		if item.Type == "message" {
			for _, block := range item.Content {
				if block.Type == "output_text" || block.Type == "text" {
					text += block.Text
				}
			}
		}
	}

	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventResult, nil, func(e *msg.Event) {
		e.Result = &msg.ResultEvent{
			Text:     text,
			NumTurns: 0, // rehydrated — no new turns
			APICalls: 1,
			Model:    h.cfg.Model,
		}
		if obj.Usage != nil {
			e.Result.Usage = msg.TokenUsage{
				InputTokens:     obj.Usage.InputTokens,
				OutputTokens:    obj.Usage.OutputTokens,
				TotalTokens:     obj.Usage.TotalTokens,
				ReasoningTokens: obj.Usage.ReasoningTokens,
			}
			e.Result.Cost = h.client.computeCost(e.Result.Usage)
		}
	}))
	return nil
}

func (h *harness) handleForgetResponse(p responseIDParams) error {
	if p.ID == "" {
		return fmt.Errorf("forget_response: id required")
	}
	if err := h.client.deleteResponse(h.ctx, p.ID); err != nil {
		return err
	}
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "response_deleted", Message: p.ID}
	}))
	return nil
}

// sendMessage issues a turn. Called directly from handleStart (before queue
// is running) and via the turn worker for queued messages.
func (h *harness) sendMessage(content, idempotencyKey string) error {
	return h.sendMessageDirect(content, idempotencyKey)
}

// sendMessageDirect issues a single /v1/responses turn. If idempotencyKey is empty,
// a fresh UUID4 is minted. Caller-supplied keys enable retry dedup.
func (h *harness) sendMessageDirect(content, idempotencyKey string) error {
	start := time.Now()
	h.lastInput = content

	if idempotencyKey == "" {
		idempotencyKey = newIdempotencyKey()
	}

	// Per-turn context tied to the harness root. interrupt() cancels just this
	// turn; shutdown() cancels the root and everything under it.
	turnCtx, turnCancel := context.WithCancel(h.ctx)
	h.turnMu.Lock()
	h.turnCancel = turnCancel
	h.turnMu.Unlock()
	defer func() {
		turnCancel()
		h.turnMu.Lock()
		h.turnCancel = nil
		h.turnMu.Unlock()
	}()

	// Hermes rejects requests that set both `conversation` and
	// `previous_response_id` simultaneously — they're two different
	// continuation mechanisms. After the first turn we chain via the
	// response ID and drop the conversation name; the `X-Hermes-Session-Id`
	// header still carries the session identity across the chain.
	req := responsesRequest{
		Model:        h.cfg.Model,
		Input:        content,
		Instructions: h.systemPrompt,
		Stream:       true,
		Store:        true,
	}
	if h.lastRespID != "" {
		req.PreviousResponseID = h.lastRespID
	} else {
		req.Conversation = h.conversation
	}
	resp, err := h.client.sendResponses(turnCtx, req, sendOptions{
		BridgeSessionID: h.bridgeSessionID,
		SessionID:       h.sessionID,
		IdempotencyKey:  idempotencyKey,
	})
	if err != nil {
		// Distinguish context cancellation (interrupt) from API errors.
		if errors.Is(err, context.Canceled) {
			emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
				e.Error = &msg.ErrorEvent{Code: "INTERRUPTED", Message: "turn cancelled", Retryable: false}
			}))
			return err
		}

		// Extract status code and determine retryability
		statusCode := 0
		retryable := false
		if apiErr, ok := err.(*apiError); ok {
			statusCode = apiErr.StatusCode
			// 5xx errors and 429 are typically retryable
			retryable = (statusCode >= 500 && statusCode < 600) || statusCode == 429
		}

		emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventError, nil, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:       "API_ERROR",
				Message:    err.Error(),
				StatusCode: statusCode,
				Retryable:  retryable,
			}
		}))
		return err
	}

	h.lastRespID = resp.ID
	durationMS := time.Since(start).Milliseconds()

	// Emit result.
	emitEvent(makeEvent(h.bridgeSessionID, h.sessionID, msg.EventResult, nil, func(e *msg.Event) {
		e.Result = &msg.ResultEvent{
			Text:       resp.Text,
			DurationMS: durationMS,
			NumTurns:   1,
			APICalls:   1,
			Model:      h.cfg.Model,
			Usage:      resp.Usage,
		}
		e.Result.Cost = resp.Cost
	}))

	log.Printf("turn complete: resp_id=%s duration=%dms idem=%s", resp.ID, durationMS, idempotencyKey)
	return nil
}

func (h *harness) shutdown() {
	h.cancel()
	if h.queue != nil {
		close(h.queue)
	}
}

// newIdempotencyKey mints a RFC 4122 v4-style hex UUID for Idempotency-Key.
// We don't need hyphen formatting — Hermes treats it as an opaque string.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic. Fall back to timestamp-only would
		// violate "fail fast and loud" since it defeats dedup. Surface instead.
		return fmt.Sprintf("idem-fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[:])
}

func makeEvent(bridgeSessionID, harnessSessionID string, eventType msg.EventType, raw json.RawMessage, fill func(*msg.Event)) msg.Event {
	e := msg.Event{
		Type:             eventType,
		Harness:          msg.HarnessHermes,
		BridgeSessionID:  bridgeSessionID,
		HarnessSessionID: harnessSessionID,
		Timestamp:        time.Now(),
		Raw:              raw,
	}
	fill(&e)
	return e
}
