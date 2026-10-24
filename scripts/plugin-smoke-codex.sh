#!/usr/bin/env bash
# Install the plugin into a throwaway CODEX_HOME and
# check the MCP server starts and fleet_status is callable, with hooks left
# untrusted. Needs: codex >= 0.147 on PATH, OPENAI_API_KEY, kenaz on PATH.
# Never touches your real ~/.codex.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
export CODEX_HOME="$(mktemp -d)"
trap 'rm -rf "$CODEX_HOME"' EXIT
printf '%s' "${OPENAI_API_KEY:?set OPENAI_API_KEY}" | codex login --with-api-key
codex plugin marketplace add "$repo"
codex plugin add kenaz@kenaz   # Codex has no `plugin install` (0.140 through 0.164 checked)
codex exec --skip-git-repo-check "Call the kenaz fleet_status tool and print its raw result." | tee /dev/stderr | grep -qi 'fleet'
echo "plugin-smoke-codex: ok"
