#!/usr/bin/env bash
# Build llm-bridge-hermes and install it as the binary bridge-server spawns.
#
# bridge-server resolves a harness by name through exec.LookPath
# (llm-bridge-server internal/harness/manager.go), so $HOME/bin/llm-bridge-hermes
# IS the deployment. Every session spawns a fresh process, which is why there is
# no service to restart here: the next session started after this script runs
# gets the new binary, and sessions already in flight keep the one they started
# with.
#
# The boot smoke runs BEFORE the install, not after. A harness that panics on
# startup compiles perfectly and fails at session-spawn time, where nobody is
# watching; installing first would put that binary in front of live sessions and
# then tell us about it.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge-hermes"
USER_BIN="$HOME/bin/$BIN_NAME"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"

echo "==> Testing $BIN_NAME..."
go vet ./...
go test ./...

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" .
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

# Checked BEFORE the install: an unidentifiable binary compiles perfectly and reads
# clean in the log, so installing first would put it in front of live sessions and
# only then tell us it cannot be traced back to a commit.
echo "==> Checking provenance..."
buildinfo="$(go version -m "$BIN_NAME")"
vcs_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
vcs_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
    echo "    REFUSING TO INSTALL: this binary carries no vcs.revision, so nothing can tie" >&2
    echo "    it back to a commit. 'go build' writes no VCS stamp when it cannot find a .git" >&2
    echo "    DIRECTORY, and it does not fail when that happens -- not even with -buildvcs=true." >&2
    echo "    The usual cause is building from a git worktree, whose .git is a pointer file." >&2
    echo "    Build from a real clone or checkout instead." >&2
    exit 1
fi
echo "    vcs.revision=$vcs_revision"
if [ "$vcs_modified" = "true" ]; then
    echo "    WARNING: built from a DIRTY tree (vcs.modified=true). $vcs_revision names the" >&2
    echo "    commit this binary was built NEAR, not the source it was built FROM, and that" >&2
    echo "    source is not recoverable from any commit. Commit first for a reproducible build." >&2
fi

echo "==> Boot smoke (scripts/e2e-smoke.sh)..."
bash scripts/e2e-smoke.sh >/dev/null
echo "    boots, discovers and answers"

echo "==> Installing to $USER_BIN..."
mkdir -p "$HOME/bin"
cp "$BIN_NAME" "$USER_BIN"

echo "==> Verifying..."
"$USER_BIN" -version
echo "    hermes API: ${HERMES_URL:-http://localhost:8642} $(curl -sfS -o /dev/null -m 2 "${HERMES_URL:-http://localhost:8642}/v1/models" 2>/dev/null && echo '(reachable)' || echo '(not reachable from here — sessions resolve it at start)')"

echo "==> Done. bridge-server picks up the new binary on the next session spawn (no restart needed)."
