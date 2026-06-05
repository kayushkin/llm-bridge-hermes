package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// apiError wraps HTTP errors with status code for better error reporting.
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	return e.Message
}

// hermesClient talks to the Hermes API server.
type hermesClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	cfg        Config
}

func newHermesClient(cfg Config) *hermesClient {
	return &hermesClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		httpClient: &http.Client{},
		cfg:        cfg,
	}
}

// computeCost calculates the cost in USD based on token usage and pricing config.
func (c *hermesClient) computeCost(usage msg.TokenUsage) *msg.Cost {
	inputCost := (float64(usage.InputTokens) / 1_000_000.0) * c.cfg.InputPricePerM
	outputCost := (float64(usage.OutputTokens) / 1_000_000.0) * c.cfg.OutputPricePerM
	reasoningCost := (float64(usage.ReasoningTokens) / 1_000_000.0) * c.cfg.ReasoningPricePerM

	total := inputCost + outputCost + reasoningCost

	return &msg.Cost{
		TotalUSD:  total,
		InputUSD:  inputCost,
		OutputUSD: outputCost + reasoningCost, // Reasoning tokens count as output
	}
}

// responsesRequest is the body for POST /v1/responses.
type responsesRequest struct {
	Model              string           `json:"model"`
	Input              string           `json:"input"`
	Instructions       string           `json:"instructions,omitempty"`
	Stream             bool             `json:"stream"`
	Store              bool             `json:"store"`
	Conversation       string           `json:"conversation,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Tools              []toolDefinition `json:"tools,omitempty"`
	ToolChoice         *toolChoice      `json:"tool_choice,omitempty"`
	Temperature        *float64         `json:"temperature,omitempty"`
	TopP               *float64         `json:"top_p,omitempty"`
	MaxOutputTokens    *int             `json:"max_output_tokens,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
	Metadata           map[string]any   `json:"metadata,omitempty"`
}

type toolDefinition struct {
	Type         string         `json:"type"` // "function"
	Function     functionSchema `json:"function"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
}

type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
	Strict      bool            `json:"strict,omitempty"`
}

type toolChoice struct {
	Type     string `json:"type"` // "auto", "none", "required", "function"
	Function *struct {
		Name string `json:"name"`
	} `json:"function,omitempty"`
}

type reasoningConfig struct {
	Enabled bool   `json:"enabled"`
	Budget  *int   `json:"budget,omitempty"` // Token budget for reasoning
	Effort  string `json:"effort,omitempty"` // "low", "medium", "high"
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// responsesResult holds the accumulated result from a streamed /v1/responses call.
type responsesResult struct {
	ID    string
	Text  string
	Usage msg.TokenUsage
	Cost  *msg.Cost
}

// sendOptions are per-call knobs layered onto an HTTP request to Hermes.
type sendOptions struct {
	// BridgeSessionID is bridge-server's stable session id. It is stamped onto
	// every event emitted from this call's SSE stream so consumers can route by
	// it, and is sent as the X-Hermes-Session-Id header for native session
	// continuity (Hermes treats it as opaque). The bridge id is the stable
	// identity that is always present, so it is the correlation key.
	BridgeSessionID string
	// HarnessSessionID is the harness-native session id, stamped onto every
	// emitted event. Hermes does not surface its own id, so this is empty unless
	// a caller explicitly supplied one (resume). It is never used for the
	// X-Hermes-Session-Id header and must never equal BridgeSessionID — the
	// server contract rejects HarnessSessionID == BridgeSessionID.
	HarnessSessionID string
	// IdempotencyKey dedupes retries server-side (5-minute cache). Required —
	// callers must mint one (typically UUID4) so retries don't double-bill.
	IdempotencyKey string
}

// sendResponses calls POST /v1/responses with streaming and translates SSE events
// into canonical msg.Events emitted via emitEvent. Returns the final result.
func (c *hermesClient) sendResponses(ctx context.Context, req responsesRequest, opts sendOptions) (*responsesResult, error) {
	if opts.IdempotencyKey == "" {
		return nil, fmt.Errorf("sendResponses: IdempotencyKey required")
	}
	if opts.BridgeSessionID == "" {
		return nil, fmt.Errorf("sendResponses: BridgeSessionID required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/responses", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", opts.IdempotencyKey)
	httpReq.Header.Set("X-Hermes-Session-Id", opts.BridgeSessionID)
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, &apiError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("hermes API error: %d %s", resp.StatusCode, string(b)),
		}
	}

	return c.parseSSEStream(resp.Body, opts.BridgeSessionID, opts.HarnessSessionID)
}

// doJSON performs an HTTP request and decodes the JSON response body.
// Used for non-streaming Hermes endpoints (/v1/models, /v1/responses/{id}, /health).
func (c *hermesClient) doJSON(ctx context.Context, method, path string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return &apiError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("hermes API error: %s %s -> %d %s", method, path, resp.StatusCode, string(b)),
		}
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// modelsResponse is the /v1/models payload shape (OpenAI-compatible).
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// listModels queries GET /v1/models and returns the advertised model IDs in order.
func (c *hermesClient) listModels(ctx context.Context) ([]string, error) {
	var out modelsResponse
	if err := c.doJSON(ctx, "GET", "/v1/models", &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// healthCheck calls GET /v1/health and returns nil on 200.
func (c *hermesClient) healthCheck(ctx context.Context) error {
	return c.doJSON(ctx, "GET", "/v1/health", nil)
}

// getResponse retrieves a stored response by ID.
func (c *hermesClient) getResponse(ctx context.Context, id string) (*responseObject, error) {
	if id == "" {
		return nil, fmt.Errorf("getResponse: id required")
	}
	var out responseObject
	if err := c.doJSON(ctx, "GET", "/v1/responses/"+id, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deleteResponse removes a stored response by ID.
func (c *hermesClient) deleteResponse(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("deleteResponse: id required")
	}
	return c.doJSON(ctx, "DELETE", "/v1/responses/"+id, nil)
}

// parseSSEStream reads SSE events from the Hermes streaming response and
// emits canonical events. Returns the accumulated result.
func (c *hermesClient) parseSSEStream(r io.Reader, bridgeSessionID, harnessSessionID string) (*responsesResult, error) {
	result := &responsesResult{}
	var textBuilder strings.Builder

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:]
		if data == "[DONE]" {
			break
		}

		var event sseEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			log.Printf("parse SSE event: %v", err)
			continue
		}

		raw := json.RawMessage(data)
		c.translateEvent(event, bridgeSessionID, harnessSessionID, result, &textBuilder, raw)
	}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("SSE stream error: %w", err)
	}

	result.Text = textBuilder.String()
	return result, nil
}

// sseEvent is a single SSE event from the Hermes /v1/responses stream.
// Hermes follows the OpenAI Responses API streaming format.
type sseEvent struct {
	Type string `json:"type"`

	// response.created / response.completed / response.failed / response.cancelled / response.incomplete
	Response *responseObject `json:"response,omitempty"`

	// response.output_item.added / done
	Item *outputItem `json:"item,omitempty"`

	// response.output_text.delta / done
	Delta        string `json:"delta,omitempty"`
	Text         string `json:"text,omitempty"`
	OutputIndex  int    `json:"output_index,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`

	// response.function_call_arguments.delta / done
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// response.reasoning.delta / done (thinking/reasoning blocks)
	Reasoning string `json:"reasoning,omitempty"`

	// response.refusal.delta / done
	Refusal string `json:"refusal,omitempty"`

	// response.content_part.added / done
	Part *contentPart `json:"part,omitempty"`

	// rate_limits.updated
	RateLimits *rateLimits `json:"rate_limits,omitempty"`

	// error information
	Error *errorDetail `json:"error,omitempty"`
}

type responseObject struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Output []outputItem `json:"output,omitempty"`
	Usage  *usageObject `json:"usage,omitempty"`
}

type outputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Output is the function_call_output payload. Hermes emits it as either
	// a plain string OR an array of content parts (e.g. `[{"type":"input_text","text":"..."}]`).
	// RawMessage lets us accept either shape without losing data; extractOutputText
	// normalises to a string for downstream consumers.
	Output  json.RawMessage `json:"output,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content []contentBlock  `json:"content,omitempty"`
}

// extractOutputText normalises the Output field of a function_call_output item
// into a plain string. Hermes may send either a bare string or an array of
// typed content parts; the bridge passes text through without reshaping.
func extractOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Then try an array of content parts.
	var parts []contentBlock
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	// Unknown shape — surface as raw JSON rather than silently dropping.
	return string(raw)
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type usageObject struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	TotalTokens     int `json:"total_tokens"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type contentPart struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Text  string `json:"text,omitempty"`
}

type rateLimits struct {
	RequestsLimit     int `json:"requests_limit,omitempty"`
	RequestsRemaining int `json:"requests_remaining,omitempty"`
	TokensLimit       int `json:"tokens_limit,omitempty"`
	TokensRemaining   int `json:"tokens_remaining,omitempty"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (c *hermesClient) translateEvent(event sseEvent, bridgeSessionID, harnessSessionID string, result *responsesResult, text *strings.Builder, raw json.RawMessage) {
	switch event.Type {
	case "response.created":
		if event.Response != nil {
			result.ID = event.Response.ID
		}

	case "response.output_text.delta":
		text.WriteString(event.Delta)
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventStream, raw, func(e *msg.Event) {
			e.Stream = &msg.HarnessStream{
				Delta: &msg.BlockDelta{
					Index: event.ContentIndex,
					Type:  msg.DeltaText,
					Text:  event.Delta,
				},
			}
		}))

	case "response.output_text.done":
		// Finished text content block — emit as EventBlock per the
		// canonical contract (EventStream is reserved for the deltas above).
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventBlock, raw, func(e *msg.Event) {
			e.Block = &msg.BlockEvent{
				Index: event.ContentIndex,
				Block: &msg.ContentBlock{
					Type: msg.BlockText,
					Text: &msg.TextBlock{Text: event.Text},
				},
			}
		}))

	case "response.reasoning.delta":
		// Streaming thinking/reasoning content
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventStream, raw, func(e *msg.Event) {
			e.Stream = &msg.HarnessStream{
				Delta: &msg.BlockDelta{
					Index:    event.ContentIndex,
					Type:     msg.DeltaThinking,
					Thinking: event.Reasoning,
				},
			}
		}))

	case "response.reasoning.done":
		// Finished thinking content block — emit as EventBlock per the
		// canonical contract (EventStream is reserved for the deltas above).
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventBlock, raw, func(e *msg.Event) {
			e.Block = &msg.BlockEvent{
				Index: event.ContentIndex,
				Block: &msg.ContentBlock{
					Type:     msg.BlockThinking,
					Thinking: &msg.ThinkingBlock{Text: event.Reasoning},
				},
			}
		}))

	case "response.refusal.delta":
		// Streaming refusal content — emit as error stream
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventStream, raw, func(e *msg.Event) {
			e.Stream = &msg.HarnessStream{
				Delta: &msg.BlockDelta{
					Index: event.ContentIndex,
					Type:  msg.DeltaText,
					Text:  event.Refusal,
				},
			}
		}))

	case "response.refusal.done":
		// Model refused the request
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventError, raw, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:    "MODEL_REFUSAL",
				Message: event.Refusal,
			}
		}))

	case "response.content_part.added":
		// Content block started — lifecycle tracking
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
			partType := "unknown"
			if event.Part != nil {
				partType = event.Part.Type
			}
			e.System = &msg.SystemEvent{
				Subtype: "content_part_added",
				Message: fmt.Sprintf("content part started: type=%s index=%d", partType, event.ContentIndex),
			}
		}))

	case "response.content_part.done":
		// Content block finished
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{
				Subtype: "content_part_done",
				Message: fmt.Sprintf("content part completed: index=%d", event.ContentIndex),
			}
		}))

	case "response.output_item.added":
		if event.Item != nil {
			switch event.Item.Type {
			case "function_call":
				emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventToolCall, raw, func(e *msg.Event) {
					e.ToolCall = &msg.ToolCallEvent{
						ToolID: event.Item.CallID,
						Name:   event.Item.Name,
					}
				}))
			case "message":
				// Assistant message item started
				emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
					e.System = &msg.SystemEvent{
						Subtype: "message_item_added",
						Message: fmt.Sprintf("message item started: role=%s", event.Item.Role),
					}
				}))
			}
		}

	case "response.function_call_arguments.delta":
		// Tool input streaming — now mapped to DeltaInputJSON
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventStream, raw, func(e *msg.Event) {
			e.Stream = &msg.HarnessStream{
				Delta: &msg.BlockDelta{
					Index:       event.ContentIndex,
					Type:        msg.DeltaInputJSON,
					PartialJSON: event.Arguments,
				},
			}
		}))

	case "response.function_call_arguments.done":
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventToolCall, raw, func(e *msg.Event) {
			e.ToolCall = &msg.ToolCallEvent{
				ToolID: event.CallID,
				Name:   event.Name,
				Input:  json.RawMessage(event.Arguments),
			}
		}))

	case "response.output_item.done":
		if event.Item == nil {
			return
		}
		switch event.Item.Type {
		case "function_call_output":
			emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventToolResult, raw, func(e *msg.Event) {
				e.ToolResult = &msg.ToolResultEvent{
					ToolID: event.Item.CallID,
					Output: extractOutputText(event.Item.Output),
				}
			}))
		case "message":
			// Message item completed
			emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
				e.System = &msg.SystemEvent{
					Subtype: "message_item_done",
					Message: "message item completed",
				}
			}))
		}

	case "response.completed":
		if event.Response != nil {
			result.ID = event.Response.ID
			if event.Response.Usage != nil {
				result.Usage = msg.TokenUsage{
					InputTokens:     event.Response.Usage.InputTokens,
					OutputTokens:    event.Response.Usage.OutputTokens,
					TotalTokens:     event.Response.Usage.TotalTokens,
					ReasoningTokens: event.Response.Usage.ReasoningTokens,
				}
				// Compute cost based on usage and pricing config
				result.Cost = c.computeCost(result.Usage)
			}
		}

	case "response.failed", "response.cancelled", "response.incomplete":
		// Terminal error states
		errMsg := "response terminated"
		errCode := "RESPONSE_ERROR"
		if event.Error != nil {
			errMsg = event.Error.Message
			if event.Error.Code != "" {
				errCode = event.Error.Code
			}
		}
		if event.Response != nil && event.Response.Status != "" {
			errMsg = fmt.Sprintf("%s (status: %s)", errMsg, event.Response.Status)
		}
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventError, raw, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:      errCode,
				Message:   errMsg,
				Retryable: event.Type == "response.failed", // failed may be retryable, cancelled/incomplete are not
			}
		}))

	case "rate_limits.updated":
		// Rate limit information
		if event.RateLimits != nil {
			emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
				e.System = &msg.SystemEvent{
					Subtype: "rate_limit",
					Message: fmt.Sprintf("rate limits: %d/%d requests, %d/%d tokens",
						event.RateLimits.RequestsRemaining, event.RateLimits.RequestsLimit,
						event.RateLimits.TokensRemaining, event.RateLimits.TokensLimit),
				}
			}))
		}

	default:
		// Forward unknown event types as system events.
		emitEvent(makeEvent(bridgeSessionID, harnessSessionID, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: event.Type}
		}))
	}
}
