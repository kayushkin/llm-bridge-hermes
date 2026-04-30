package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func testClient(url string) *hermesClient {
	return &hermesClient{
		baseURL:    url,
		apiKey:     "test-key",
		httpClient: &http.Client{},
		cfg: Config{
			InputPricePerM:     3.0,
			OutputPricePerM:    15.0,
			ReasoningPricePerM: 15.0,
		},
	}
}

func TestComputeCost(t *testing.T) {
	c := testClient("")

	usage := msg.TokenUsage{
		InputTokens:     1_000_000,
		OutputTokens:    1_000_000,
		ReasoningTokens: 500_000,
	}
	cost := c.computeCost(usage)

	if cost.InputUSD != 3.0 {
		t.Errorf("InputUSD = %f, want 3.0", cost.InputUSD)
	}
	// Output = 15.0 (output) + 7.5 (reasoning) = 22.5
	if cost.OutputUSD != 22.5 {
		t.Errorf("OutputUSD = %f, want 22.5", cost.OutputUSD)
	}
	if cost.TotalUSD != 25.5 {
		t.Errorf("TotalUSD = %f, want 25.5", cost.TotalUSD)
	}
}

func TestComputeCost_Zero(t *testing.T) {
	c := testClient("")
	cost := c.computeCost(msg.TokenUsage{})
	if cost.TotalUSD != 0 {
		t.Errorf("TotalUSD = %f, want 0", cost.TotalUSD)
	}
}

func TestParseSSEStream_TextDelta(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
		`data: {"type":"response.content_part.added","part":{"type":"output_text","index":0}}`,
		`data: {"type":"response.output_text.delta","delta":"Hello ","content_index":0}`,
		`data: {"type":"response.output_text.delta","delta":"world!","content_index":0}`,
		`data: {"type":"response.output_text.done","text":"Hello world!"}`,
		`data: {"type":"response.content_part.done","content_index":0}`,
		`data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant"}}`,
		`data: {"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	result, err := c.parseSSEStream(strings.NewReader(stream), "sess-1")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	if result.ID != "resp-1" {
		t.Errorf("result.ID = %q, want resp-1", result.ID)
	}
	if result.Text != "Hello world!" {
		t.Errorf("result.Text = %q, want 'Hello world!'", result.Text)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}

	events := getEvents()
	// Should have stream deltas plus a finished EventBlock for the text.
	var streamCount, textBlockCount int
	for _, e := range events {
		if e.Type == msg.EventStream && e.Stream != nil && e.Stream.Delta != nil && e.Stream.Delta.Type == msg.DeltaText {
			streamCount++
		}
		if e.Type == msg.EventBlock && e.Block != nil && e.Block.Block != nil && e.Block.Block.Type == msg.BlockText {
			textBlockCount++
			if e.Block.Block.Text == nil || e.Block.Block.Text.Text != "Hello world!" {
				t.Errorf("text EventBlock payload = %+v, want text 'Hello world!'", e.Block.Block.Text)
			}
		}
	}
	if streamCount != 2 {
		t.Errorf("expected 2 text delta events, got %d", streamCount)
	}
	if textBlockCount != 1 {
		t.Errorf("expected 1 text EventBlock, got %d", textBlockCount)
	}
}

func TestParseSSEStream_ToolCall(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-2","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc-1","name":"read_file","call_id":"call-1"}}`,
		`data: {"type":"response.function_call_arguments.delta","arguments":"{\"path\"","content_index":0}`,
		`data: {"type":"response.function_call_arguments.delta","arguments":": \"/tmp/x\"}","content_index":0}`,
		`data: {"type":"response.function_call_arguments.done","call_id":"call-1","name":"read_file","arguments":"{\"path\": \"/tmp/x\"}"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call_output","call_id":"call-1","output":"file contents"}}`,
		`data: {"type":"response.completed","response":{"id":"resp-2","status":"completed","usage":{"input_tokens":200,"output_tokens":80,"total_tokens":280}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	result, err := c.parseSSEStream(strings.NewReader(stream), "sess-2")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if result.ID != "resp-2" {
		t.Errorf("result.ID = %q, want resp-2", result.ID)
	}

	events := getEvents()

	// Should have tool_call events.
	var toolCalls, toolResults, inputJSONDeltas int
	for _, e := range events {
		if e.Type == msg.EventToolCall {
			toolCalls++
		}
		if e.Type == msg.EventToolResult {
			toolResults++
		}
		if e.Type == msg.EventStream && e.Stream != nil && e.Stream.Delta != nil && e.Stream.Delta.Type == msg.DeltaInputJSON {
			inputJSONDeltas++
		}
	}
	// output_item.added (function_call) + arguments.done = 2 tool call events
	if toolCalls != 2 {
		t.Errorf("expected 2 tool_call events, got %d", toolCalls)
	}
	if toolResults != 1 {
		t.Errorf("expected 1 tool_result event, got %d", toolResults)
	}
	if inputJSONDeltas != 2 {
		t.Errorf("expected 2 input_json delta events, got %d", inputJSONDeltas)
	}
}

func TestParseSSEStream_ToolCall_ArrayOutput(t *testing.T) {
	// Hermes sometimes emits `output` as an array of content parts instead
	// of a bare string. The bridge must accept both without dropping events.
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-arr","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc-1","name":"read","call_id":"call-1"}}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"part one "},{"type":"input_text","text":"part two"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp-arr","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	if _, err := c.parseSSEStream(strings.NewReader(stream), "sess-arr"); err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	events := getEvents()
	var toolResult *msg.ToolResultEvent
	for _, e := range events {
		if e.Type == msg.EventToolResult && e.ToolResult != nil {
			toolResult = e.ToolResult
		}
	}
	if toolResult == nil {
		t.Fatal("expected a tool_result event")
	}
	if toolResult.Output != "part one part two" {
		t.Errorf("Output = %q, want 'part one part two'", toolResult.Output)
	}
}

func TestParseSSEStream_Reasoning(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-3","status":"in_progress"}}`,
		`data: {"type":"response.reasoning.delta","reasoning":"Let me think...","content_index":0}`,
		`data: {"type":"response.reasoning.done","reasoning":"Let me think about this carefully."}`,
		`data: {"type":"response.output_text.delta","delta":"The answer is 42.","content_index":0}`,
		`data: {"type":"response.completed","response":{"id":"resp-3","status":"completed","usage":{"input_tokens":50,"output_tokens":30,"total_tokens":80,"reasoning_tokens":20}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	result, err := c.parseSSEStream(strings.NewReader(stream), "sess-3")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if result.Usage.ReasoningTokens != 20 {
		t.Errorf("ReasoningTokens = %d, want 20", result.Usage.ReasoningTokens)
	}

	events := getEvents()
	var thinkingDeltas, thinkingBlocks int
	for _, e := range events {
		if e.Type == msg.EventStream && e.Stream != nil && e.Stream.Delta != nil && e.Stream.Delta.Type == msg.DeltaThinking {
			thinkingDeltas++
		}
		if e.Type == msg.EventBlock && e.Block != nil && e.Block.Block != nil && e.Block.Block.Type == msg.BlockThinking {
			thinkingBlocks++
			if e.Block.Block.Thinking == nil || e.Block.Block.Thinking.Text != "Let me think about this carefully." {
				t.Errorf("thinking EventBlock payload = %+v, want 'Let me think about this carefully.'", e.Block.Block.Thinking)
			}
		}
	}
	if thinkingDeltas != 1 {
		t.Errorf("expected 1 thinking delta, got %d", thinkingDeltas)
	}
	if thinkingBlocks != 1 {
		t.Errorf("expected 1 thinking EventBlock, got %d", thinkingBlocks)
	}
}

func TestParseSSEStream_Refusal(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-4","status":"in_progress"}}`,
		`data: {"type":"response.refusal.delta","refusal":"I cannot help with that."}`,
		`data: {"type":"response.refusal.done","refusal":"I cannot help with that."}`,
		`data: {"type":"response.completed","response":{"id":"resp-4","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	_, err := c.parseSSEStream(strings.NewReader(stream), "sess-4")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	events := getEvents()
	var refusalErrors int
	for _, e := range events {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "MODEL_REFUSAL" {
			refusalErrors++
		}
	}
	if refusalErrors != 1 {
		t.Errorf("expected 1 refusal error event, got %d", refusalErrors)
	}
}

func TestParseSSEStream_Failed(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-5","status":"in_progress"}}`,
		`data: {"type":"response.failed","response":{"id":"resp-5","status":"failed"},"error":{"type":"server_error","code":"INTERNAL","message":"internal error"}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	_, err := c.parseSSEStream(strings.NewReader(stream), "sess-5")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	events := getEvents()
	var errorEvents int
	for _, e := range events {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "INTERNAL" {
			errorEvents++
		}
	}
	if errorEvents != 1 {
		t.Errorf("expected 1 error event, got %d", errorEvents)
	}
}

func TestParseSSEStream_RateLimits(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"rate_limits.updated","rate_limits":{"requests_limit":100,"requests_remaining":99,"tokens_limit":50000,"tokens_remaining":49000}}`,
		`data: {"type":"response.completed","response":{"id":"resp-6","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	_, err := c.parseSSEStream(strings.NewReader(stream), "sess-6")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	events := getEvents()
	var rateLimitEvents int
	for _, e := range events {
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "rate_limit" {
			rateLimitEvents++
		}
	}
	if rateLimitEvents != 1 {
		t.Errorf("expected 1 rate_limit event, got %d", rateLimitEvents)
	}
}

func TestParseSSEStream_UnknownEvent(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"hermes.custom.event","foo":"bar"}`,
		`data: {"type":"response.completed","response":{"id":"resp-7","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c := testClient("")
	getEvents := captureEvents(t)

	_, err := c.parseSSEStream(strings.NewReader(stream), "sess-7")
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}

	events := getEvents()
	var forwarded int
	for _, e := range events {
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "hermes.custom.event" {
			forwarded++
		}
	}
	if forwarded != 1 {
		t.Errorf("expected 1 forwarded unknown event, got %d", forwarded)
	}
}

func TestDoJSON_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	c := testClient(server.URL)
	var out map[string]string
	err := c.doJSON(t.Context(), "GET", "/v1/health", &out)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if out["status"] != "ok" {
		t.Errorf("status = %q, want ok", out["status"])
	}
}

func TestDoJSON_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer server.Close()

	c := testClient(server.URL)
	err := c.doJSON(t.Context(), "GET", "/v1/health", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

func TestDoJSON_NilOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testClient(server.URL)
	err := c.doJSON(t.Context(), "GET", "/health", nil)
	if err != nil {
		t.Fatalf("doJSON with nil out: %v", err)
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		json.NewEncoder(w).Encode(modelsResponse{
			Object: "list",
			Data: []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				OwnedBy string `json:"owned_by"`
			}{
				{ID: "hermes-agent", Object: "model"},
				{ID: "hermes-lite", Object: "model"},
			},
		})
	}))
	defer server.Close()

	c := testClient(server.URL)
	ids, err := c.listModels(t.Context())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "hermes-agent" || ids[1] != "hermes-lite" {
		t.Errorf("ids = %v, want [hermes-agent hermes-lite]", ids)
	}
}

func TestHealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("path = %q, want /v1/health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testClient(server.URL)
	err := c.healthCheck(t.Context())
	if err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
}

func TestGetResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/resp-123" {
			t.Errorf("path = %q, want /v1/responses/resp-123", r.URL.Path)
		}
		json.NewEncoder(w).Encode(responseObject{
			ID:     "resp-123",
			Status: "completed",
			Output: []outputItem{
				{
					Type: "message",
					Content: []contentBlock{
						{Type: "output_text", Text: "Hello from stored response"},
					},
				},
			},
			Usage: &usageObject{
				InputTokens:  100,
				OutputTokens: 50,
				TotalTokens:  150,
			},
		})
	}))
	defer server.Close()

	c := testClient(server.URL)
	resp, err := c.getResponse(t.Context(), "resp-123")
	if err != nil {
		t.Fatalf("getResponse: %v", err)
	}
	if resp.ID != "resp-123" {
		t.Errorf("ID = %q, want resp-123", resp.ID)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("Output count = %d, want 1", len(resp.Output))
	}
}

func TestGetResponse_EmptyID(t *testing.T) {
	c := testClient("")
	_, err := c.getResponse(t.Context(), "")
	if err == nil {
		t.Error("expected error for empty id")
	}
}

func TestDeleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp-456" {
			t.Errorf("path = %q, want /v1/responses/resp-456", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testClient(server.URL)
	err := c.deleteResponse(t.Context(), "resp-456")
	if err != nil {
		t.Fatalf("deleteResponse: %v", err)
	}
}

func TestSendResponses_RequiresOptions(t *testing.T) {
	c := testClient("http://localhost:0")

	_, err := c.sendResponses(t.Context(), responsesRequest{}, sendOptions{})
	if err == nil || !strings.Contains(err.Error(), "IdempotencyKey required") {
		t.Errorf("expected IdempotencyKey required error, got: %v", err)
	}

	_, err = c.sendResponses(t.Context(), responsesRequest{}, sendOptions{IdempotencyKey: "key"})
	if err == nil || !strings.Contains(err.Error(), "SessionID required") {
		t.Errorf("expected SessionID required error, got: %v", err)
	}
}

func TestSendResponses_Headers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers.
		if r.Header.Get("Idempotency-Key") != "idem-123" {
			t.Errorf("Idempotency-Key = %q, want idem-123", r.Header.Get("Idempotency-Key"))
		}
		if r.Header.Get("X-Hermes-Session-Id") != "sess-abc" {
			t.Errorf("X-Hermes-Session-Id = %q, want sess-abc", r.Header.Get("X-Hermes-Session-Id"))
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		// Verify body.
		body, _ := io.ReadAll(r.Body)
		var req responsesRequest
		json.Unmarshal(body, &req)
		if req.Instructions != "be helpful" {
			t.Errorf("Instructions = %q, want 'be helpful'", req.Instructions)
		}
		if req.Conversation != "conv-1" {
			t.Errorf("Conversation = %q, want conv-1", req.Conversation)
		}

		// Return minimal SSE stream.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r-1\",\"status\":\"in_progress\"}}\n"))
		w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n"))
		w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	c := testClient(server.URL)
	_ = captureEvents(t)

	result, err := c.sendResponses(t.Context(), responsesRequest{
		Model:        "hermes-agent",
		Input:        "hello",
		Instructions: "be helpful",
		Stream:       true,
		Store:        true,
		Conversation: "conv-1",
	}, sendOptions{
		SessionID:      "sess-abc",
		IdempotencyKey: "idem-123",
	})
	if err != nil {
		t.Fatalf("sendResponses: %v", err)
	}
	if result.ID != "r-1" {
		t.Errorf("result.ID = %q, want r-1", result.ID)
	}
}

func TestSendResponses_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	c := testClient(server.URL)
	_, err := c.sendResponses(t.Context(), responsesRequest{}, sendOptions{
		SessionID:      "sess",
		IdempotencyKey: "key",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T", err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}
