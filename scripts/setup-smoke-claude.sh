#!/usr/bin/env bash
# After `kenaz setup claude`, a headless prompt makes
# the relay receive at least one api_request log within 30 s.
# Needs: a running relay (`kenaz daemon`) whose `doctor` shows the
# received-signal timestamps, claude on PATH, credentials.
# Uses a throwaway CLAUDE_CONFIG_DIR; never touches your real ~/.claude.
set -euo pipefail
bin="${KENAZ_BIN:-kenaz}"
export CLAUDE_CONFIG_DIR="$(mktemp -d)"
trap 'rm -rf "$CLAUDE_CONFIG_DIR"' EXIT
"$bin" setup claude --yes
claude -p "say hi" >/dev/null
for _ in $(seq 30); do
  if "$bin" status 2>/dev/null | grep -q 'claude_code.api_request'; then echo "setup-smoke-claude: ok"; exit 0; fi
  sleep 1
done
echo "setup-smoke-claude: no api_request reached the relay in 30 s" >&2; exit 1
