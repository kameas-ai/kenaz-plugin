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
  cp -R "$here/opencode" "$tmp/r/"
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

fresh; mkdir -p "$tmp/r/internal/setup"
printf 'package setup\nconst x = `"permission"`\n' > "$tmp/r/internal/setup/s.go"
expect_fail "OpenCode permission key in setup" no-policy

fresh; mkdir -p "$tmp/r/internal/setup"
printf 'package setup\nconst x = `"tools"`\n' > "$tmp/r/internal/setup/s.go"
expect_fail "OpenCode tools key in setup" no-policy

fresh; mkdir -p "$tmp/r/internal/bundle"
printf 'package bundle\nconst x = "permissionDecision"\n' > "$tmp/r/internal/bundle/b.go"
expect_fail "permissionDecision in bundle renderer" no-policy

fresh; printf '\nexport const h = { "tool.execute.before": async () => {} };\n' >> "$tmp/r/opencode/src/index.js"
expect_fail "OpenCode adapter tool.execute.before" no-policy
fresh; printf '\nexport const h = { "permission.ask": async () => {} };\n' >> "$tmp/r/opencode/src/map.js"
expect_fail "OpenCode adapter permission.ask" no-policy
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["dependencies"]={"left-pad":"1.0.0"}; json.dump(d,open(p,"w"))' "$tmp/r/opencode/package.json"
expect_fail "OpenCode adapter runtime dependency" no-policy
fresh
python3 -c 'import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["name"]="@kameas/kenaz-codex-opencode"; json.dump(d,open(p,"w"))' "$tmp/r/opencode/package.json"
expect_fail "OpenCode adapter package name with a third-party mark" names

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

# Build identity values belong in internal/variant only.
idfail() {
  local label="$1" code="$2"
  fresh; mkdir -p "$tmp/r/internal/x"
  printf 'package x\n\n%s\n' "$code" > "$tmp/r/internal/x/x.go"
  expect_fail "identity literal: $label" names
}
idfail "binary name" 'var b = "kenaz"'
idfail "config dir" 'var d = filepath.Join("kameas", "kenaz")'
idfail "config dir path" 'const d = "kameas/kenaz"'
idfail "keychain service" 'const s = "kenaz-plugin"'
idfail "plugin id" 'const p = "kenaz@kenaz"'
idfail "cookie" 'const c = "kenaz_approvals_csrf"'
idfail "env var" 'const e = "KENAZ_ENV"'
idfail "relay port" 'var p = []int{4318}'
fresh; mkdir -p "$tmp/r/internal/x" "$tmp/r/internal/variant"
printf 'package x\n\n// the "kenaz" marketplace on 4318\nconst p = "@kameas/kenaz-opencode"\n' > "$tmp/r/internal/x/x.go"
printf 'package variant\n\nvar b = "kenaz"\n' > "$tmp/r/internal/variant/v.go"
printf 'package x\n\nvar b = "kenaz"\n' > "$tmp/r/internal/x/x_test.go"
python3 "$checks" names "$tmp/r" >/dev/null || { echo "FAIL: identity literals in comments, internal/variant or tests must pass" >&2; exit 1; }
echo "ok: identity literals allowed in comments, internal/variant and tests"

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
