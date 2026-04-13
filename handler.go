package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// request is the JSON-RPC request format from llm-bridge.
type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type startParams struct {
	SessionID   string `json:"session_id"`
	DisplayName string `json:"display_name,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Resume      bool   `json:"resume,omitempty"`
}

type messageParams struct {
	Content string `json:"content"`
}

type compactParams struct {
	Summary string `json:"summary,omitempty"`
}

// harness holds the runtime state for a Hermes session.
type harness struct {
	cfg        Config
	client     *hermesClient
	sessionID  string
	conversation string // Hermes conversation name for multi-turn
	lastRespID string   // last response ID for chaining
	ctx        context.Context
	cancel     context.CancelFunc
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

	default:
		return fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (h *harness) handleStart(p startParams) error {
	h.sessionID = p.SessionID
	h.conversation = p.SessionID // use session ID as Hermes conversation name

	emitEvent(makeEvent(h.sessionID, msg.EventSessionState, nil, func(e *msg.Event) {
		e.State = &msg.StateEvent{State: msg.SessionRunning, Previous: msg.SessionIdle}
	}))

	if p.Prompt == "" {
		emitEvent(makeEvent(h.sessionID, msg.EventSessionState, nil, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionIdle, Previous: msg.SessionRunning, Reason: "ready"}
		}))
		return nil
	}

	return h.sendMessage(p.Prompt)
}

func (h *harness) handleMessage(p messageParams) error {
	if h.sessionID == "" {
		return fmt.Errorf("no active session")
	}

	emitEvent(makeEvent(h.sessionID, msg.EventSessionState, nil, func(e *msg.Event) {
		e.State = &msg.StateEvent{State: msg.SessionRunning, Previous: msg.SessionIdle}
	}))

	return h.sendMessage(p.Content)
}

func (h *harness) handleCompact(p compactParams) error {
	emitEvent(makeEvent(h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "compact_ack", Message: "compaction delegated to Hermes"}
	}))
	return nil
}

func (h *harness) handleResume() error {
	// Hermes maintains server-side conversation state via the conversation name.
	// Resuming is implicit — the next message will continue the conversation.
	emitEvent(makeEvent(h.sessionID, msg.EventSystem, nil, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "resume", Message: "session resumed"}
	}))
	return nil
}

func (h *harness) sendMessage(content string) error {
	start := time.Now()

	resp, err := h.client.sendResponses(h.ctx, responsesRequest{
		Model:              h.cfg.Model,
		Input:              content,
		Stream:             true,
		Store:              true,
		Conversation:       h.conversation,
		PreviousResponseID: h.lastRespID,
	}, h.sessionID)
	if err != nil {
		// Extract status code and determine retryability
		statusCode := 0
		retryable := false
		if apiErr, ok := err.(*apiError); ok {
			statusCode = apiErr.StatusCode
			// 5xx errors and 429 are typically retryable
			retryable = (statusCode >= 500 && statusCode < 600) || statusCode == 429
		}

		emitEvent(makeEvent(h.sessionID, msg.EventError, nil, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:       "API_ERROR",
				Message:    err.Error(),
				StatusCode: statusCode,
				Retryable:  retryable,
			}
		}))
		emitEvent(makeEvent(h.sessionID, msg.EventSessionState, nil, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionError, Previous: msg.SessionRunning, Reason: err.Error()}
		}))
		return err
	}

	h.lastRespID = resp.ID
	durationMS := time.Since(start).Milliseconds()

	// Emit result.
	emitEvent(makeEvent(h.sessionID, msg.EventResult, nil, func(e *msg.Event) {
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

	emitEvent(makeEvent(h.sessionID, msg.EventSessionState, nil, func(e *msg.Event) {
		e.State = &msg.StateEvent{State: msg.SessionIdle, Previous: msg.SessionRunning}
	}))

	log.Printf("turn complete: resp_id=%s duration=%dms", resp.ID, durationMS)
	return nil
}

func (h *harness) shutdown() {
	h.cancel()
}

func makeEvent(sessionID string, eventType msg.EventType, raw json.RawMessage, fill func(*msg.Event)) msg.Event {
	e := msg.Event{
		Type:      eventType,
		Harness:   msg.HarnessHermes,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Raw:       raw,
	}
	fill(&e)
	return e
}
