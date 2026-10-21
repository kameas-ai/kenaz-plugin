#!/usr/bin/env bash
# Install the plugin from this checkout's marketplace into a
# throwaway Claude Code config dir and check the MCP server connects.
# Needs: claude >= 2.1.217 on PATH, an authenticated ANTHROPIC_API_KEY (or
# copy credentials into $CLAUDE_CONFIG_DIR yourself), kenaz on PATH.
# Never touches your real ~/.claude.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
export CLAUDE_CONFIG_DIR="$(mktemp -d)"
trap 'rm -rf "$CLAUDE_CONFIG_DIR"' EXIT
claude plugin marketplace add "$repo"
claude plugin install kenaz@kenaz
claude mcp list | tee /dev/stderr | grep -q 'kenaz.*[Cc]onnected'
claude -p "Call the kenaz fleet_status tool and print its raw result." | tee /dev/stderr | grep -qi 'fleet'
echo "plugin-smoke-claude: ok"
