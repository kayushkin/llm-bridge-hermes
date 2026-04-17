#!/usr/bin/env bash
# Install a local Hermes Agent and point it at Anthropic so the bridge's
# integration tests have a real server to hit.
#
# Idempotent: re-running updates config, restarts the gateway, and leaves
# the installed repo + venv in place. Fails fast on any error.
#
# Environment inputs:
#   ANTHROPIC_API_KEY   required. Real key with Anthropic API credit.
#                       Falls back to model-store's anthropic:api row when unset.
#   HERMES_MODEL        default: claude-haiku-4-5
#   HERMES_PORT         default: 8642
#
# Outputs:
#   A running `hermes gateway` on http://127.0.0.1:$HERMES_PORT
#   Verified with `curl /v1/health`
#
# After setup:
#   cd llm-bridge-hermes && go test -tags=integration ./...

set -euo pipefail

HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
HERMES_REPO="$HERMES_HOME/hermes-agent"
HERMES_PORT="${HERMES_PORT:-8642}"
HERMES_HOST="${HERMES_HOST:-127.0.0.1}"
HERMES_MODEL_NAME="${HERMES_MODEL:-claude-haiku-4-5}"

log()  { printf '\033[1;34m[setup-hermes]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[setup-hermes]\033[0m %s\n' "$*" >&2; exit 1; }

# ── Resolve the Anthropic key ─────────────────────────────────────────────────
if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  if command -v sqlite3 >/dev/null && [[ -f "$HOME/.config/model-store/store.db" ]]; then
    ANTHROPIC_API_KEY="$(sqlite3 "$HOME/.config/model-store/store.db" \
      "SELECT api_key FROM credentials WHERE id='anthropic:api' AND auth_type='api_key' LIMIT 1" 2>/dev/null || true)"
  fi
fi
[[ -n "${ANTHROPIC_API_KEY:-}" ]] || fail "ANTHROPIC_API_KEY is not set and model-store has no anthropic:api row"
[[ "$ANTHROPIC_API_KEY" == sk-ant-api* ]] || fail "ANTHROPIC_API_KEY does not look like an API key (expected sk-ant-api prefix)"

# ── Install uv if missing ─────────────────────────────────────────────────────
export PATH="$HOME/.local/bin:$PATH"
if ! command -v uv >/dev/null; then
  log "Installing uv..."
  curl -LsSf https://astral.sh/uv/install.sh | sh
fi

# ── Clone + build hermes-agent (minimal extras) ───────────────────────────────
if [[ ! -d "$HERMES_REPO/.git" ]]; then
  log "Cloning hermes-agent into $HERMES_REPO..."
  mkdir -p "$HERMES_HOME"
  git clone --depth 1 https://github.com/NousResearch/hermes-agent.git "$HERMES_REPO"
fi

cd "$HERMES_REPO"
if [[ ! -x "venv/bin/hermes" ]]; then
  log "Creating Python 3.11 venv..."
  uv venv venv --python 3.11
fi

log "Installing hermes-agent with api-server extras (web,cron,dev,mcp)..."
# shellcheck disable=SC1091
source venv/bin/activate
uv pip install -e '.[web,cron,dev,mcp]' >/dev/null

# ── Configure Hermes for headless use with Anthropic ──────────────────────────
mkdir -p "$HERMES_HOME"

log "Writing $HERMES_HOME/config.yaml (provider=anthropic, model=$HERMES_MODEL_NAME)..."
cat > "$HERMES_HOME/config.yaml" <<YAML
model:
  provider: anthropic
  default: $HERMES_MODEL_NAME
YAML

log "Writing $HERMES_HOME/.env (API server + ANTHROPIC_TOKEN)..."
# ANTHROPIC_TOKEN short-circuits hermes' resolve_anthropic_token() so the
# api_key from the auth pool isn't silently replaced with a Claude Code
# OAuth token from ~/.claude/.credentials.json right before each request.
cat > "$HERMES_HOME/.env" <<ENV
API_SERVER_ENABLED=true
API_SERVER_HOST=$HERMES_HOST
API_SERVER_PORT=$HERMES_PORT
ANTHROPIC_TOKEN=$ANTHROPIC_API_KEY
ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
ENV
chmod 600 "$HERMES_HOME/.env"

log "Registering anthropic api_key credential in Hermes auth store..."
hermes auth add anthropic --type api-key --api-key "$ANTHROPIC_API_KEY" --label "integration-tests" >/dev/null 2>&1 || true

# Suppress competing credential sources (Claude Code OAuth, Hermes PKCE)
# so the manual api_key is the only one Hermes can pick up.
python3 - <<'PY'
import sys
sys.path.insert(0, ".")
from hermes_cli.auth import suppress_credential_source
for src in ("claude_code", "hermes_pkce", "env:CLAUDE_CODE_OAUTH_TOKEN", "env:ANTHROPIC_TOKEN"):
    suppress_credential_source("anthropic", src)
print("[setup-hermes] suppressed competing anthropic credential sources")
PY

# ── Start the gateway (restart if already running) ────────────────────────────
if pgrep -f "hermes gateway run" >/dev/null; then
  log "Stopping existing gateway..."
  pkill -9 -f "hermes gateway run" || true
  sleep 2
fi
rm -f "$HERMES_HOME/gateway.pid"

log "Starting gateway on http://$HERMES_HOST:$HERMES_PORT..."
nohup hermes gateway run -v --replace > "$HERMES_HOME/logs/gateway.log" 2>&1 &
disown

# ── Probe /v1/health until ready ──────────────────────────────────────────────
for i in $(seq 1 30); do
  if curl -fsS "http://$HERMES_HOST:$HERMES_PORT/v1/health" >/dev/null 2>&1; then
    log "Gateway healthy on http://$HERMES_HOST:$HERMES_PORT (attempt $i)"
    break
  fi
  sleep 1
  if [[ $i -eq 30 ]]; then
    tail -20 "$HERMES_HOME/logs/gateway.log" >&2
    fail "Gateway did not become healthy within 30s"
  fi
done

# ── Smoke test — confirm Anthropic backend is actually answering ──────────────
log "Smoke-testing /v1/responses..."
resp="$(curl -fsS -N -X POST "http://$HERMES_HOST:$HERMES_PORT/v1/responses" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: setup-smoke-$$" \
  -H "X-Hermes-Session-Id: setup-smoke" \
  -d "{\"model\":\"hermes-agent\",\"input\":\"Reply with exactly PONG\",\"stream\":true,\"store\":false,\"max_output_tokens\":16}")"
if ! echo "$resp" | grep -q "response.completed"; then
  echo "$resp" | tail -5 >&2
  fail "Smoke test did not see response.completed"
fi
if ! echo "$resp" | grep -q '"error"'; then
  log "Smoke test passed — real Anthropic conversation confirmed."
else
  echo "$resp" | grep -m1 '"error"' >&2
  fail "Smoke test produced an error event"
fi

log "Done. Run integration tests with:"
log "  cd llm-bridge-hermes && go test -tags=integration -v ./..."
