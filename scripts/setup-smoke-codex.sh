#!/usr/bin/env bash
# After `kenaz setup codex`, `codex exec`
# makes the relay receive at least one codex.api_request within 30 s.
# Needs: a running relay, codex on PATH, OPENAI_API_KEY.
# Uses a throwaway CODEX_HOME; never touches your real ~/.codex.
set -euo pipefail
bin="${KENAZ_BIN:-kenaz}"
export CODEX_HOME="$(mktemp -d)"
trap 'rm -rf "$CODEX_HOME"' EXIT
printf '%s' "${OPENAI_API_KEY:?set OPENAI_API_KEY}" | codex login --with-api-key
"$bin" setup codex --yes
codex exec --skip-git-repo-check "say hi" >/dev/null
for _ in $(seq 30); do
  if "$bin" status 2>/dev/null | grep -q 'codex.api_request'; then echo "setup-smoke-codex: ok"; exit 0; fi
  sleep 1
done
echo "setup-smoke-codex: no codex.api_request reached the relay in 30 s" >&2; exit 1
