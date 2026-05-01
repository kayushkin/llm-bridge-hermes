# llm-bridge-hermes

Harness bridge for the [Hermes Agent](https://hermes-agent.nousresearch.com/) API. Translates between the llm-bridge subprocess protocol (NDJSON on stdin/stdout) and Hermes' OpenAI-compatible `/v1/responses` endpoint with SSE streaming.

## Build

This module uses local `replace` directives for `github.com/kayushkin/llm-bridge` and `github.com/kayushkin/aiauth`, so both repos must be checked out next to this one:

```
repos/
├── aiauth/
├── llm-bridge/
└── llm-bridge-hermes/
```

Then:

```bash
go build -o llm-bridge-hermes
```

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

All variables are optional — the bridge starts with safe defaults and discovers what it can.

| Variable | Description |
|----------|-------------|
| `HERMES_URL` | Hermes API server base URL (default: `http://localhost:8642`) |
| `HERMES_API_KEY` | Bearer token for Hermes API authentication. Lower precedence than `LLMBRIDGE_CREDENTIAL_ID` and the `start` message's `credential_id` |
| `HERMES_MODEL` | Model name for requests. If unset, auto-discovered via `GET /v1/models` |
| `HERMES_PREFLIGHT` | Set to `1` to run a health check on startup before accepting requests |
| `HERMES_DASHBOARD_URL` | Hermes web dashboard URL (e.g. `http://127.0.0.1:9119`). Required for `-discover` |
| `HERMES_DASHBOARD_KEY` | Auth token for the dashboard API |
| `HERMES_INPUT_PRICE_PER_M` | USD per million input tokens (default: `3.0`) |
| `HERMES_OUTPUT_PRICE_PER_M` | USD per million output tokens (default: `15.0`) |
| `HERMES_REASONING_PRICE_PER_M` | USD per million reasoning tokens (default: `15.0`) |
| `LLMBRIDGE_CREDENTIAL_ID` | aiauth profile name to resolve into the bearer token. Set by llm-bridge-server when launching this harness; can also be set manually for standalone runs. Overridden by the `start` message's `credential_id` field |

## JSON-RPC Methods

| Method | Description |
|--------|-------------|
| `start` | Initialize session. Params: `session_id`, `prompt`, `system_prompt`, `model`, `credential_id` (optional, overrides `LLMBRIDGE_CREDENTIAL_ID`), `display_name`, `agent_id`, `resume` |
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

## Testing

Unit tests run against stubbed SSE streams and an `httptest.Server`:

```bash
go test ./...
```

Integration tests run against a **real** Hermes Agent on
`http://127.0.0.1:8642` with a real Anthropic backend. Use the setup
script to install Hermes, wire it to your Anthropic key, start the
gateway, and verify a live smoke test:

```bash
# Installs hermes-agent into ~/.hermes/hermes-agent, writes config,
# starts the gateway, and sends one /v1/responses request to prove
# the Anthropic backend is answering end-to-end.
./scripts/setup-hermes.sh

# Then:
go test -tags=integration -v ./...
```

The script reads `ANTHROPIC_API_KEY` from the environment; if unset,
it falls back to model-store (`~/.config/model-store/store.db`,
`anthropic:api` row). Hermes picks up `claude-haiku-4-5` by default
— override with `HERMES_MODEL=<name>`.

Integration tests fail fast (not skip) if the gateway isn't
reachable, because running them is a deliberate choice.

## Fork semantics

Hermes fork is **per-response**, not per-session. The Hermes server has no primitive to clone a conversation by name — only to chain a new turn off a specific `previous_response_id`. There are two ways forks reach this bridge, and they are handled differently:

1. **Bridge-server `start{fork: <parent_harness_session_id>}`** (the canonical session-fork wire) — **rejected** with `EventError{Code: "FORK_UNSUPPORTED", Retryable: false}`. Faking a fresh chain under a new conversation name would silently drop all parent state and pretend a fork happened, violating the contract. Callers that need a session-level fork should use a harness that supports it natively (e.g. claudecode, jig).
2. **Explicit `fork` JSON-RPC method with `from_response_id`** — supported. Rebases the response chain onto the supplied `from_response_id`; the next `message()` will use it as parent instead of `lastRespID`. An optional `conversation` parameter switches the conversation name as well.

In short: callers that hold a *response* id can fork; callers that hold only a *session* id cannot.

## Known Gaps

- **File/vision upload**: Blocked upstream (Hermes API doesn't support it yet)
- **Compact/compress**: No HTTP endpoint; Hermes handles this server-side via `/compress` slash command
- **Permission mode**: Hermes auto-approves all tools on the API path; the bridge cannot gate tool execution
- **Chat Completions**: Only `/v1/responses` is used; `/v1/chat/completions` is not implemented
- **Idempotency-Key dedup on streaming**: The bridge sends `Idempotency-Key` on every request, but Hermes only consults its idempotency cache on the non-streaming `/v1/responses` branch. Streaming requests always run fresh. Retries with the same key will currently double-charge.
