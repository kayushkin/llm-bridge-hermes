# Hermes Bridge — Gap Analysis & Extension Plan

This document inventories Hermes Agent API surface area that is **not yet exposed** through `llm-bridge-hermes`, and proposes how to add each piece.

References:
- Hermes API Server docs: https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server
- Hermes Web Dashboard API: https://hermes-agent.nousresearch.com/docs/user-guide/features/web-dashboard
- Hermes Cron: https://hermes-agent.nousresearch.com/docs/user-guide/features/cron
- Unofficial ref: https://github.com/mudrii/hermes-agent-docs
- Current bridge: `client.go`, `handler.go`, `config.go` (v0.1.0)

---

## 1. What the bridge already handles

Transport
- `POST /v1/responses` with `stream: true` and SSE translation
- Bearer auth via `HERMES_API_KEY`
- Pricing-based cost computation (input/output/reasoning per-M USD)

Request fields sent
- `model`, `input`, `instructions`, `stream`, `store`, `conversation`, `previous_response_id`
- `tools` (function schemas with optional `cache_control`), `tool_choice`
- `temperature`, `top_p`, `max_output_tokens`
- `reasoning { enabled, budget, effort }`, `metadata`

SSE events decoded
- `response.created | completed | failed | cancelled | incomplete`
- `response.output_text.delta | done`
- `response.reasoning.delta | done`
- `response.refusal.delta | done`
- `response.content_part.added | done`
- `response.output_item.added | done` (function_call, message)
- `response.function_call_arguments.delta | done`
- `rate_limits.updated`

JSON-RPC methods accepted on stdin
- `start`, `message`, `compact` (no-op ack), `resume` (no-op ack)

Canonical events emitted
- `stream`, `thinking`, `tool_call`, `tool_result`, `result`, `error`, `system`, `session_state`

Declared capability flags in server health: `["model"]` (see `llm-bridge-server/internal/server/health.go:60`)

---

## 2. Gap inventory

Each gap below lists: **what Hermes exposes**, **what the bridge does today**, and **proposed bridging approach**. Priority labels: P0 (blocker for parity), P1 (high value), P2 (nice-to-have), P3 (exposed via side-channel only).

### 2.1 Response lifecycle endpoints

| Gap | Hermes | Bridge today | Plan |
|---|---|---|---|
| **Retrieve stored response** | `GET /v1/responses/{id}` returns a persisted response object (text, tool items, usage). | Unused. After `response.completed`, the ID is kept in `h.lastRespID` but never fetched. | **P2.** Add `client.getResponse(ctx, id)`. Expose as a new JSON-RPC method `get_response` that re-emits the stored response as a fresh `result` event. Useful for rehydrating dropped streams. |
| **Delete stored response** | `DELETE /v1/responses/{id}` removes a persisted response. | Unused. Responses LRU-evict server-side at 100. | **P2.** Add `client.deleteResponse(ctx, id)`. Expose as JSON-RPC `forget_response { id }` and as part of session shutdown if the bridge is asked to purge history. |
| **List / discover models** | `GET /v1/models` advertises the agent's name (profile-configurable). | `cfg.Model` is hardcoded via env (`HERMES_MODEL`, default `hermes-agent`). Can drift from server's advertised name. | **P1.** On first `start`, call `client.listModels(ctx)` and use the first advertised model if `HERMES_MODEL` is unset. Emit `system { subtype: "model_discovered", message: <id> }`. Never silently overwrite an explicit env var. |
| **Health probe** | `GET /health` and `GET /v1/health` return `{"status":"ok"}`. | No preflight. First request failure surfaces as generic API error. | **P2.** Add an optional startup health check (toggled by `HERMES_PREFLIGHT=1`). Emit `system { subtype: "preflight_ok" }` or `error { code: "PREFLIGHT_FAIL" }` before accepting `message` requests. |

### 2.2 Request body fields not plumbed

| Gap | Hermes | Bridge today | Plan |
|---|---|---|---|
| **System prompt via `instructions`** | `instructions` layers on top of Hermes' core prompt. | `handleStart` only forwards `p.Prompt` as `input`; `instructions` is always empty. The agent-store may have a SOUL/system prompt intended for this slot. | **P0.** Extend `startParams` with `system_prompt` / `instructions` (match whatever `llm-bridge-server` sends; see `msg.StartPayload` or equivalent). Store on `harness` and send as `instructions` on every `sendResponses` call (Hermes layers it each turn). |
| **Chat Completions fallback** | `POST /v1/chat/completions` is the stateless format — full messages array per request. | Not used. | **P3.** Skip unless we need stateless replay (e.g., when `previous_response_id` is unavailable or for testing). Track as a backlog item; no action now. |
| **Multimodal input** | Docs explicitly state **file/vision upload "not yet supported"** on the API server. | Bridge only accepts `content string`. | **Blocked upstream.** Keep `msg.ContentBlock` plumbing in `startParams`/`messageParams` inert until Hermes ships support. No code changes now, but add a TODO in `handler.go` near `messageParams` so we remember to wire `ImageBlock`/`DocumentBlock` when available. |
| **Idempotency-Key header** | Server caches responses by `Idempotency-Key` for 5 minutes. | Never sent. Retries will create duplicate responses / double-bill. | **P1.** Generate a key per outgoing request (UUID4) and attach `Idempotency-Key: <uuid>` in `sendResponses`. Allow override via `messageParams.IdempotencyKey` for caller-driven dedup. Log the key to stderr for debugging. |
| **`X-Hermes-Session-Id` header** | Optional native session continuity. | Not sent. Bridge currently uses `session_id` as the `conversation` name (which works but couples two concerns). | **P1.** Send `X-Hermes-Session-Id: <h.sessionID>` on every request. Keep `conversation` too — they serve different purposes (the header continues a Hermes-side session; `conversation` groups related responses). Document the overlap in `README.md` when we add one. |
| **`n`, `seed`, `stop`, `logprobs`** | Standard OpenAI params. Availability on Hermes unverified; docs don't list them. | Not plumbed. | **P2.** Add optional fields to `responsesRequest` gated by config env vars (`HERMES_SEED`, etc.). Only send when set. Verify support against a live Hermes before shipping. |

### 2.3 Control plane methods

The bridge accepts a minimal JSON-RPC surface. Sister harnesses (`llm-bridge-claudecode/handler.go:132-170`) support `interrupt`, `set_model`, `set_permission_mode`, `control`. Hermes deserves parity for these where the underlying agent supports it.

| Gap | Hermes equivalent | Bridge today | Plan |
|---|---|---|---|
| **Interrupt / stop in-flight turn** | `/stop` slash command (user-facing). No documented HTTP cancel; `ctx` cancel on the HTTP request aborts the stream. | `shutdown()` cancels the root context, killing the whole process. No per-turn cancel. | **P0.** Split contexts: keep a root `h.ctx` plus a per-turn `h.turnCancel`. Add JSON-RPC `interrupt` that calls `h.turnCancel()`. On cancellation, emit `state { aborted }` and `error { code: "INTERRUPTED", retryable: false }`. Also propagate SIGINT from stdin close to just the turn, not the whole process, until explicit shutdown. |
| **Set model at runtime** | `/model` slash command + per-request `model` field (cosmetic server-side but respected by the request log). | `cfg.Model` is immutable after `loadConfig()`. | **P1.** Add `set_model { model: string }` JSON-RPC method. Guard against empty string (fail fast per CLAUDE.md). Emit `system { subtype: "model_changed", message: <new> }`. Use the new model on the next `sendMessage`. |
| **Compress / compact** | `/compress` compresses context server-side. No documented HTTP trigger — it's a conversation slash-command. | `handleCompact` returns a fake ack, does nothing. This is a lie. | **P1.** Two paths:<br>1. Send `/compress` as the user `input` with `store: true`. Relies on Hermes interpreting slash commands through the API — **verify against live server first**.<br>2. If (1) isn't supported, surface an honest error: replace the fake ack with `error { code: "UNSUPPORTED", message: "compact not available on Hermes HTTP API" }`. Don't pretend to have compacted.<br>Report the capability accurately in `harnessCapabilities` (see §4). |
| **Fork / branch** | Create divergent turns by reusing an earlier `previous_response_id`. | No way to specify which parent to branch from. | **P2.** Add `fork { from_response_id: string }` JSON-RPC; next `message` uses that ID instead of `h.lastRespID`. Optionally create a new `conversation` name so the branch is logically separate. |
| **Permission mode / tool approval** | `/permission` slash command; smart approvals learn safe commands. Not exposed on the HTTP API. | Bridge never emits `approval` events because Hermes handles approvals server-side. | **P3.** Leave as-is. Document that Hermes auto-approves on the API path ("API server gives full access to hermes-agent's toolset, including terminal commands") — the bridge can't gate tools. |
| **Retry / undo** | `/retry` and `/undo` slash commands. | No equivalent. | **P2.** Implement `retry` JSON-RPC as "resend the last user input with a fresh `Idempotency-Key`, reusing the previous parent response ID". Leave `undo` as backlog (needs `delete_response` from §2.1 plus tracking of the prior ID). |
| **Queue prompt** | `/queue` appends without interrupting. | No queue; concurrent `message` calls would race. | **P2.** Serialize turns inside the harness with a `chan messageParams` + worker goroutine. JSON-RPC `message` enqueues; existing turn finishes first. This is a bridge-side concern, not a Hermes API feature. |
| **Personality** | `/personality [name]`. | Not exposed. | **P3.** Treat as cosmetic; ignore. |

### 2.4 SSE events not handled

| Gap | Plan |
|---|---|
| `hermes.tool.progress` (Chat Completions stream only) | Only relevant if we add the `/v1/chat/completions` path (§2.2). Skip. |
| Missing `response.*` types that Hermes may add in future | Keep the `default` case's system-event forwarding. Already implemented — good. |
| **Prompt logprobs / token-level metadata**, if emitted | Add passthrough via `Extensions` on the canonical event. Defer until we see one in the wild. |

### 2.5 Dashboard API (port 9119, separate from `/v1`)

The web dashboard exposes a REST API that the OpenAI-compatible path does not duplicate. These are adjacent to the bridge's core job but some are valuable.

| Endpoint | Value for bridge | Plan |
|---|---|---|
| `GET /api/sessions`, `/api/sessions/{id}`, `/api/sessions/{id}/messages` | Lets llm-bridge-server discover native Hermes sessions (like it does for Claude Code under `~/.claude/projects/`). | **P1.** Add a `hermes_discover` CLI subcommand or new method `list_stored_sessions` that returns `msg.StoredSession` entries by calling this API. Wire into `llm-bridge-server`'s session discovery. |
| `GET /api/sessions/search?q=` | FTS across Hermes memory. | **P2.** Expose as optional JSON-RPC `search_history { query }`. |
| `GET /api/analytics/usage?days=N` | Totals for cost/tokens over N days. | **P2.** Expose on a new `/v1/bridge/analytics` extension. Not urgent — bridge computes per-session cost locally. |
| `GET /api/cron/jobs`, `POST /api/cron/jobs`, `POST /api/cron/jobs/{id}/pause\|resume\|trigger`, `DELETE /api/cron/jobs/{id}` | Hermes has its own scheduler. This project already has a separate `scheduler` service. | **P3.** Leave to the `scheduler` project; don't duplicate. Document which is the system of record (see open questions, §5). |
| `GET /api/skills`, `PUT /api/skills/toggle` | Skill catalog management. | **P3.** Out of scope for the bridge; belongs in a Hermes-specific admin UI. |
| `GET /api/logs` | Structured log access. | **P3.** Covered by `logstack` for our deployment. |
| `GET/PUT /api/config`, `/api/env` | Config management (includes secrets). | **Do not expose.** Security surface. If ever needed, fence behind a dedicated admin bridge. |

Because the dashboard lives on a **different port** (9119) and uses **cookie or unauthenticated local access**, plumbing it requires a second base URL + optional auth config. Recommended env vars:
- `HERMES_DASHBOARD_URL` (default `http://127.0.0.1:9119`, empty = dashboard features disabled)
- `HERMES_DASHBOARD_KEY` (if/when Hermes adds dashboard auth)

### 2.6 Bridge metadata / server integration

| Gap | Plan |
|---|---|
| Capability advertisement in `llm-bridge-server/internal/server/health.go:60` declares only `["model"]`. | After implementing §2.3 items, update the list. Target: `{"model","compact","fork","effort","tools","system_prompt","interrupt"}` — only include what actually works against a live Hermes. |
| No README in the bridge repo. | Add one summarising env vars, supported/unsupported features, and the `/v1` vs dashboard split. |

---

## 3. Proposed implementation order

Grouped so each phase is independently shippable and testable against a live Hermes.

**Phase 1 — Correctness of what already exists (P0)**
1. Wire `instructions` through `startParams` → `harness.systemPrompt` → `responsesRequest.Instructions`. (§2.2)
2. Split contexts and add `interrupt` JSON-RPC method. (§2.3)
3. Replace the fake `compact` ack with either a real `/compress` passthrough or an honest `UNSUPPORTED` error after verifying against a live server. (§2.3)

**Phase 2 — Request hygiene (P1)**
4. Send `Idempotency-Key` on every request; accept caller override. (§2.2)
5. Send `X-Hermes-Session-Id` header. (§2.2)
6. Add `set_model` JSON-RPC method. (§2.3)
7. Call `GET /v1/models` once at startup when `HERMES_MODEL` is unset. (§2.1)
8. Update capabilities in `llm-bridge-server` `health.go` to reflect new methods. (§2.6)

**Phase 3 — Session introspection (P1/P2)**
9. Dashboard client (`dashboard.go`) using `HERMES_DASHBOARD_URL`; implement `list_stored_sessions` → `msg.StoredSession`. (§2.5)
10. `GET /v1/responses/{id}` as `get_response` JSON-RPC. (§2.1)
11. `DELETE /v1/responses/{id}` as `forget_response` JSON-RPC. (§2.1)

**Phase 4 — Branching & retry (P2)**
12. `fork` JSON-RPC with explicit parent response ID. (§2.3)
13. `retry` JSON-RPC reusing prior parent ID + new idempotency key. (§2.3)
14. Turn queue / serializer for concurrent `message` calls. (§2.3)

**Phase 5 — Optional enrichment (P2/P3)**
15. `search_history` via `/api/sessions/search`. (§2.5)
16. `GET /health` preflight behind env flag. (§2.1)
17. Optional params (`seed`, `stop`, `logprobs`) gated by env vars. (§2.2)

---

## 4. File-level changes (sketch)

- `config.go`
  - Add `DashboardURL`, `DashboardKey`, `Preflight bool`, `Seed *int`, `StopSequences []string`.
  - Leave pricing unchanged.
- `client.go`
  - New helpers: `getResponse(ctx, id)`, `deleteResponse(ctx, id)`, `listModels(ctx)`, `healthCheck(ctx)`.
  - `sendResponses` gains an `idempotencyKey string` param and a caller-provided header map; sets `Idempotency-Key` and `X-Hermes-Session-Id`.
  - New `dashboardClient` struct in `dashboard.go` with `ListSessions`, `GetSession`, `SearchSessions`, `GetUsage`.
- `handler.go`
  - `startParams` gains `SystemPrompt string`.
  - `harness` gains `systemPrompt string`, `turnCancel context.CancelFunc`, `turnMu sync.Mutex`, `queue chan messageParams`.
  - New cases in `handleRequest`: `interrupt`, `set_model`, `fork`, `retry`, `get_response`, `forget_response`, `list_stored_sessions`, `search_history`.
  - `handleCompact` replaced per §2.3 item 3.
- New `doc.go` or `README.md` describing env vars, supported methods, and known gaps (file upload, permission mode).
- `llm-bridge-server/internal/server/health.go:60` — update `HarnessHermes` capabilities list after each phase lands.

---

## 5. Open questions before writing code

1. **Does Hermes' `/v1/responses` honor slash commands (e.g., `/compress`) in `input`?** If not, §2.3's compact plan changes. Test by sending `input: "/compress"` against a live server.
2. **Does Hermes return structured errors when `Idempotency-Key` collides with a changed body?** Determines whether we must track body hashes or trust the server to reject.
3. **Is the dashboard API (port 9119) ever exposed remotely in our deployments, or only via loopback?** Affects whether `HERMES_DASHBOARD_URL` needs TLS handling.
4. **Authoritative scheduler:** our `scheduler` service or Hermes `/api/cron/jobs`? Pick one before plumbing cron — do not let both schedule the same work (duplicate firings, drift).
5. **Profile support:** should a single bridge process bind to a specific Hermes profile, or negotiate it per session? Currently cfg is process-wide. Revisit when multi-profile deployments become real.

---

## 6. Non-goals

- Reimplementing Hermes' skills, memory, or MEMORY.md management from the bridge. These are agent-internal and should stay that way.
- Exposing `/api/env` or `/api/config` writes — credential and config management belongs in a dedicated admin path, not the chat bridge.
- Supporting `/v1/chat/completions` unless a concrete caller needs stateless mode. `/v1/responses` is strictly more capable for our use case.
- File/vision upload — blocked upstream until Hermes ships support.
