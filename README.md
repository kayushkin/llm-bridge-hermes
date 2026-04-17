# llm-bridge-hermes

Harness bridge for the [Hermes Agent](https://hermes-agent.nousresearch.com/) API. Translates between the llm-bridge subprocess protocol (NDJSON on stdin/stdout) and Hermes' OpenAI-compatible `/v1/responses` endpoint with SSE streaming.

## Usage

```bash
# Normal mode — reads JSON-RPC requests from stdin, emits NDJSON events to stdout.
llm-bridge-hermes

# Print version.
llm-bridge-hermes -version

# Discover stored sessions via the Hermes dashboard API.
# Requires HERMES_DASHBOARD_URL.
llm-bridge-hermes -discover
```

## Environment Variables

### Required

| Variable | Description |
|----------|-------------|
| `HERMES_URL` | Hermes API server base URL (default: `http://localhost:8642`) |

### Optional

| Variable | Description |
|----------|-------------|
| `HERMES_API_KEY` | Bearer token for Hermes API authentication |
| `HERMES_MODEL` | Model name for requests. If unset, auto-discovered via `GET /v1/models` |
| `HERMES_PREFLIGHT` | Set to `1` to run a health check on startup before accepting requests |
| `HERMES_DASHBOARD_URL` | Hermes web dashboard URL (e.g. `http://127.0.0.1:9119`). Required for `-discover` |
| `HERMES_DASHBOARD_KEY` | Auth token for the dashboard API |
| `HERMES_INPUT_PRICE_PER_M` | USD per million input tokens (default: `3.0`) |
| `HERMES_OUTPUT_PRICE_PER_M` | USD per million output tokens (default: `15.0`) |
| `HERMES_REASONING_PRICE_PER_M` | USD per million reasoning tokens (default: `15.0`) |

## JSON-RPC Methods

| Method | Description |
|--------|-------------|
| `start` | Initialize session. Params: `session_id`, `prompt`, `system_prompt`, `model` |
| `message` | Send a user message. Params: `content`, `idempotency_key` (optional) |
| `interrupt` | Cancel the in-flight turn (aborts SSE stream) |
| `resume` | Resume a session (implicit via Hermes server-side state) |
| `compact` | Returns `UNSUPPORTED` error (Hermes manages compression server-side) |
| `set_model` | Change model at runtime. Params: `model` |
| `fork` | Branch from an earlier response. Params: `from_response_id`, `conversation` (optional) |
| `retry` | Resend the last user input with a fresh idempotency key |
| `get_response` | Retrieve a stored response by ID. Params: `id` |
| `forget_response` | Delete a stored response by ID. Params: `id` |

## Canonical Events Emitted

`stream`, `thinking`, `tool_call`, `tool_result`, `result`, `error`, `system`, `session_state`

## Architecture

- **Turn serialization**: Concurrent `message` calls are queued and processed sequentially to prevent races.
- **Per-turn contexts**: Each turn gets its own cancellable context. `interrupt` cancels just the active turn; `shutdown` cancels everything.
- **Idempotency**: Every request includes an `Idempotency-Key` header (UUID4). Callers can provide their own key for retry dedup.
- **Session continuity**: Both `X-Hermes-Session-Id` and `conversation` name are sent on every request.

## Known Gaps

- **File/vision upload**: Blocked upstream (Hermes API doesn't support it yet)
- **Compact/compress**: No HTTP endpoint; Hermes handles this server-side via `/compress` slash command
- **Permission mode**: Hermes auto-approves all tools on the API path; the bridge cannot gate tool execution
- **Chat Completions**: Only `/v1/responses` is used; `/v1/chat/completions` is not implemented
