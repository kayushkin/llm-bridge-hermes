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

// hermesClient talks to the Hermes API server.
type hermesClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newHermesClient(cfg Config) *hermesClient {
	return &hermesClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		httpClient: &http.Client{},
	}
}

// responsesRequest is the body for POST /v1/responses.
type responsesRequest struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	Instructions       string `json:"instructions,omitempty"`
	Stream             bool   `json:"stream"`
	Store              bool   `json:"store"`
	Conversation       string `json:"conversation,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// responsesResult holds the accumulated result from a streamed /v1/responses call.
type responsesResult struct {
	ID    string
	Text  string
	Usage msg.TokenUsage
	Cost  *msg.Cost
}

// sendResponses calls POST /v1/responses with streaming and translates SSE events
// into canonical msg.Events emitted via emitEvent. Returns the final result.
func (c *hermesClient) sendResponses(ctx context.Context, req responsesRequest, sessionID string) (*responsesResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/responses", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("hermes API error: %d %s", resp.StatusCode, string(b))
	}

	return c.parseSSEStream(resp.Body, sessionID)
}

// parseSSEStream reads SSE events from the Hermes streaming response and
// emits canonical events. Returns the accumulated result.
func (c *hermesClient) parseSSEStream(r io.Reader, sessionID string) (*responsesResult, error) {
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
		c.translateEvent(event, sessionID, result, &textBuilder, raw)
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

	// response.created / response.completed
	Response *responseObject `json:"response,omitempty"`

	// response.output_item.added / done
	Item *outputItem `json:"item,omitempty"`

	// response.output_text.delta
	Delta      string `json:"delta,omitempty"`
	OutputIndex int   `json:"output_index,omitempty"`
	ContentIndex int  `json:"content_index,omitempty"`

	// response.function_call_arguments.delta / done
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responseObject struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Output []outputItem `json:"output,omitempty"`
	Usage  *usageObject `json:"usage,omitempty"`
}

type outputItem struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Arguments string         `json:"arguments,omitempty"`
	Output    string         `json:"output,omitempty"`
	Role      string         `json:"role,omitempty"`
	Content   []contentBlock `json:"content,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type usageObject struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (c *hermesClient) translateEvent(event sseEvent, sessionID string, result *responsesResult, text *strings.Builder, raw json.RawMessage) {
	switch event.Type {
	case "response.created":
		if event.Response != nil {
			result.ID = event.Response.ID
		}

	case "response.output_text.delta":
		text.WriteString(event.Delta)
		emitEvent(makeEvent(sessionID, msg.EventStream, raw, func(e *msg.Event) {
			e.Stream = &msg.HarnessStream{
				Delta: &msg.BlockDelta{
					Index: event.ContentIndex,
					Type:  msg.DeltaText,
					Text:  event.Delta,
				},
			}
		}))

	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "function_call" {
			emitEvent(makeEvent(sessionID, msg.EventToolCall, raw, func(e *msg.Event) {
				e.ToolCall = &msg.ToolCallEvent{
					ToolID: event.Item.CallID,
					Name:   event.Item.Name,
				}
			}))
		}

	case "response.function_call_arguments.delta":
		// Tool input streaming — not mapped to a canonical event.
		// Available in raw for consumers.

	case "response.function_call_arguments.done":
		emitEvent(makeEvent(sessionID, msg.EventToolCall, raw, func(e *msg.Event) {
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
			emitEvent(makeEvent(sessionID, msg.EventToolResult, raw, func(e *msg.Event) {
				e.ToolResult = &msg.ToolResultEvent{
					ToolID: event.Item.CallID,
					Output: event.Item.Output,
				}
			}))
		}

	case "response.completed":
		if event.Response != nil {
			result.ID = event.Response.ID
			if event.Response.Usage != nil {
				result.Usage = msg.TokenUsage{
					InputTokens:  event.Response.Usage.InputTokens,
					OutputTokens: event.Response.Usage.OutputTokens,
					TotalTokens:  event.Response.Usage.TotalTokens,
				}
			}
		}

	default:
		// Forward unknown event types as system events.
		emitEvent(makeEvent(sessionID, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: event.Type}
		}))
	}
}
