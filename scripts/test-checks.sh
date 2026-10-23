#!/usr/bin/env bash
# Self-test for scripts/checks.py: seeded violations must fail, the repo
# must pass.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
checks="$here/scripts/checks.py"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fresh() {
  rm -rf "$tmp/r" && mkdir -p "$tmp/r"
  cp -R "$here/plugin" "$here/.claude-plugin" "$tmp/r/"
  mkdir -p "$tmp/r/internal/mcp"
  cp -R "$here/internal/mcp/tools" "$tmp/r/internal/mcp/"
  mkdir -p "$tmp/r/cmd/kenaz"
}

expect_fail() {
  local label="$1"; shift
  if python3 "$checks" "$@" "$tmp/r" >/dev/null 2>&1; then
    echo "FAIL: expected violation: $label" >&2; exit 1
  fi
  echo "ok: caught $label"
}

fresh; python3 "$checks" no-policy "$tmp/r" >/dev/null
fresh; python3 "$checks" names "$tmp/r" >/dev/null
fresh; python3 "$checks" legacy-name "$tmp/r" >/dev/null

fresh
python3 - "$tmp/r/plugin/hooks/hooks.json" <<'EOF'
import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["hooks"]["PreToolUse"]=d["hooks"]["SessionStart"]; json.dump(d,open(p,"w"))
EOF
expect_fail "PreToolUse hook" no-policy

fresh; mkdir -p "$tmp/r/plugin/bin"; expect_fail "plugin/bin" no-policy
fresh; echo '{}' > "$tmp/r/plugin/settings.json"; expect_fail "plugin/settings.json" no-policy

fresh; mkdir -p "$tmp/r/internal/hook"
printf 'package hook\nconst x = "permissionDecision"\n' > "$tmp/r/internal/hook/h.go"
expect_fail "permissionDecision in hook" no-policy

fresh; mkdir -p "$tmp/r/internal/setup"
printf 'package setup\nconst x = "requirements.toml"\n' > "$tmp/r/internal/setup/s.go"
expect_fail "requirements.toml in setup" no-policy

fresh; mkdir -p "$tmp/r/internal/setup"
printf 'package setup\nconst x = "mcp_servers"\n' > "$tmp/r/internal/setup/s.go"
expect_fail "mcp_servers in setup" no-policy

fresh; mkdir -p "$tmp/r/internal/setup"
printf 'package setup\nconst x = `"hooks"`\n' > "$tmp/r/internal/setup/s.go"
expect_fail "hooks key in setup" no-policy

fresh
python3 - "$tmp/r/plugin/.claude-plugin/plugin.json" <<'EOF'
import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["name"]="kameas-claude"; json.dump(d,open(p,"w"))
EOF
expect_fail "plugin name kameas-claude" names

# Codex overlay: "approve" would auto-approve in Codex.
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["mcpServers"]["kenaz"]["tools"]["skills_publish"]["approval_mode"]="approve"; json.dump(d,open(p,"w"))' "$tmp/r/plugin/.codex-mcp.json"
expect_fail "codex overlay approve" no-policy
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["mcpServers"]["kenaz"]["enabled_tools"]=["fleet_status"]; json.dump(d,open(p,"w"))' "$tmp/r/plugin/.codex-mcp.json"
expect_fail "codex overlay enabled_tools" no-policy
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["version"]="9.9.9"; json.dump(d,open(p,"w"))' "$tmp/r/plugin/.codex-plugin/plugin.json"
expect_fail "codex overlay version drift" no-policy
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["mcpServers"]["kenaz"]["tools"]["no_such_tool"]={"approval_mode":"prompt"}; json.dump(d,open(p,"w"))' "$tmp/r/plugin/.codex-mcp.json"
expect_fail "codex overlay unknown tool" no-policy

fresh; echo '{"name":"@kameas/codex-bridge"}' > "$tmp/r/package.json"; expect_fail "npm @kameas/codex-bridge" names
fresh; cp "$tmp/r/plugin/commands/login.md" "$tmp/r/plugin/commands/openai-login.md"; expect_fail "command openai-login" names

# The former product name must not come back outside the allowlisted lines.
old="kameas-""fleet"
fresh; printf 'Run `%s login`.\n' "$old" >> "$tmp/r/plugin/README.md"; expect_fail "former name in docs" legacy-name
fresh; printf 'package x\nconst e = "KAMEAS_""FLEET_ENV"\n' | sed 's/""//' > "$tmp/r/internal/mcp/tools/x.go"; expect_fail "former env var" legacy-name
fresh; mkdir -p "$tmp/r/cmd/$old"; touch "$tmp/r/cmd/$old/main.go"; expect_fail "former binary dir" legacy-name
fresh; mkdir -p "$tmp/r/internal/project"; printf 'package project\nconst ServiceName = "%s"\n' "$old" > "$tmp/r/internal/project/encode.go"; python3 "$checks" legacy-name "$tmp/r" >/dev/null
echo "ok: allowlisted wire constant passes"

# Release verifier regression fixtures.
bash "$here/scripts/test-verify-release.sh"

echo "test-checks: ok"
