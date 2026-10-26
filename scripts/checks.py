#!/usr/bin/env python3
"""Repository policy gates.

    checks.py no-policy [ROOT]   the plugin, the OpenCode adapter and setup
                                 add no policy, plus the Codex-only
                                 manifest overlay
    checks.py names [ROOT]       no harness vendor names in our own names,
                                 and no build identity value spelled out
                                 outside internal/variant
    checks.py legacy-name [ROOT] the former product name stays gone, except
                                 for the allowlisted wire constants below

Each prints its violations and exits 1 if there are any.
"""
import json
import pathlib
import re
import sys

ALLOWED_HOOK_EVENTS = {"SessionStart", "SessionEnd"}

# Strings that would make the plugin a policy surface if they appeared in
# hook handlers (permission decisions, input rewriting, blocking).
HOOK_POLICY_TOKENS = [
    "permissionDecision",
    "updatedInput",
    '"decision"',
    '"continue"',
]

# Keys and files that setup must never write.
SETUP_POLICY_TOKENS = [
    '"permissions"',
    "requirements.toml",
    "execpolicy",
    "prefix_rule",
    "allowedMcpServers",
    "deniedMcpServers",
    "enabled_tools",
    "disabled_tools",
    "allowManagedPermissionRulesOnly",
    # Setup also never writes hooks, rules, MCP servers or
    # allow-lists. It preserves whatever the user has without naming it.
    '"hooks"',
    "[rules]",
    "mcp_servers",
    '"allowed_',
    # OpenCode: setup registers the kenaz MCP server and the adapter, and
    # never writes OpenCode's tool switches or permission rules.
    '"tools"',
    '"permission"',
]

# OpenCode server-plugin hooks that could change what OpenCode does (block
# or rewrite tool calls, edit prompts, answer permission requests, add
# tools). The adapter only observes events, so none may appear in it.
OPENCODE_POLICY_TOKENS = [
    "permission.ask",
    "tool.execute.before",
    "tool.execute.after",
    "tool.definition",
    "chat.message",
    "chat.params",
    "chat.headers",
    "command.execute.before",
    "shell.env",
    "experimental.",
    "auth:",
    "tool:",
]

BANNED_NAME = re.compile(r"claude|codex|anthropic|openai", re.IGNORECASE)

# Names the harnesses mandate. These are exempt from the name lint by
# explicit listing, never by pattern.
EXEMPT_NAMES = {
    ".claude-plugin",
    ".codex-plugin",
}


def load_json(path, errs):
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        return None
    except json.JSONDecodeError as e:
        errs.append(f"{path}: invalid JSON: {e}")
        return None


def walk_keys(obj):
    if isinstance(obj, dict):
        for k, v in obj.items():
            yield k
            yield from walk_keys(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from walk_keys(v)


def go_sources(d):
    if not d.is_dir():
        return []
    return [p for p in d.rglob("*.go") if not p.name.endswith("_test.go")]


# The Codex-only overlay may set exactly this per-tool approval mode on our
# own write tools. "approve" would AUTO-approve in Codex (codex-rs
# core/src/mcp_tool_call.rs requires_mcp_tool_approval_for_mode), so only
# "prompt" is allowed.
CODEX_OVERLAY_APPROVAL = "prompt"
CODEX_OVERLAY_SERVER_KEYS = {"command", "args", "tools"}


def check_codex_overlay(root, errs):
    """plugin/.codex-plugin/plugin.json is the manifest Codex prefers over
    .claude-plugin/plugin.json (codex-rs DISCOVERABLE_PLUGIN_MANIFEST_PATHS).
    It must mirror the Claude manifest (all but the description) and point
    at .codex-mcp.json, which must launch the same server and may only
    add approval_mode=prompt for tools this repo declares."""
    plugin = root / "plugin"
    overlay = plugin / ".codex-plugin" / "plugin.json"
    if not overlay.exists():
        return
    om = load_json(overlay, errs)
    cm = load_json(plugin / ".claude-plugin" / "plugin.json", errs)
    if om is None or cm is None:
        return
    # The descriptions differ on purpose (each names only the tools that
    # read it); TestPluginIdentity pins both exactly.
    for k in ("name", "version", "license"):
        if om.get(k) != cm.get(k):
            errs.append(f"plugin/.codex-plugin/plugin.json: {k!r} differs from .claude-plugin/plugin.json")
    if om.get("mcpServers") != "./.codex-mcp.json":
        errs.append('plugin/.codex-plugin/plugin.json: mcpServers must be "./.codex-mcp.json"')
    for k in ("hooks", "skills"):
        if k in om:
            errs.append(f"plugin/.codex-plugin/plugin.json: must not set {k!r} (Codex defaults to the shared plugin/{k})")
    cx = load_json(plugin / ".codex-mcp.json", errs)
    cl = load_json(plugin / ".mcp.json", errs)
    if cx is None or cl is None:
        return
    cxs = cx.get("mcpServers") or {}
    cls = cl.get("mcpServers") or {}
    if set(cxs) != set(cls):
        errs.append("plugin/.codex-mcp.json: server names differ from plugin/.mcp.json")
    declared = "\n".join(src.read_text() for src in go_sources(root / "internal" / "mcp" / "tools"))
    for name, srv in cxs.items():
        base = cls.get(name) or {}
        extra = set(srv) - CODEX_OVERLAY_SERVER_KEYS
        if extra:
            errs.append(f"plugin/.codex-mcp.json {name}: unexpected keys {sorted(extra)} (no tool allow/deny lists)")
        if srv.get("command") != base.get("command") or srv.get("args") != base.get("args"):
            errs.append(f"plugin/.codex-mcp.json {name}: command/args differ from plugin/.mcp.json")
        for tool, cfg in (srv.get("tools") or {}).items():
            if set(cfg) != {"approval_mode"} or cfg.get("approval_mode") != CODEX_OVERLAY_APPROVAL:
                errs.append(f"plugin/.codex-mcp.json {name}.{tool}: only approval_mode={CODEX_OVERLAY_APPROVAL!r} is allowed")
            if f'"{tool}"' not in declared:
                errs.append(f"plugin/.codex-mcp.json {name}.{tool}: no such tool in internal/mcp/tools")


def check_opencode_adapter(root, errs):
    """opencode/ is an observe-only OpenCode server plugin with no runtime
    dependencies (it must load with npm registry access blocked)."""
    adapter = root / "opencode"
    if not adapter.is_dir():
        return
    for src in sorted((adapter / "src").rglob("*.[jt]s")):
        text = src.read_text()
        for tok in OPENCODE_POLICY_TOKENS:
            if tok in text:
                errs.append(f"{src.relative_to(root)}: the OpenCode adapter must not use {tok!r} (observe-only)")
    pkg = load_json(adapter / "package.json", errs)
    if pkg is not None:
        for k in ("dependencies", "peerDependencies", "optionalDependencies", "bundledDependencies"):
            if pkg.get(k):
                errs.append(f"opencode/package.json: {k} not allowed (the adapter ships without runtime dependencies)")


def check_no_policy(root):
    errs = []
    plugin = root / "plugin"
    check_codex_overlay(root, errs)
    check_opencode_adapter(root, errs)

    hooks = load_json(plugin / "hooks" / "hooks.json", errs)
    if hooks is not None:
        events = set((hooks.get("hooks") or {}).keys())
        extra = events - ALLOWED_HOOK_EVENTS
        if extra:
            errs.append(f"plugin/hooks/hooks.json: forbidden hook events {sorted(extra)} (only {sorted(ALLOWED_HOOK_EVENTS)})")

    if (plugin / "settings.json").exists():
        errs.append("plugin/settings.json must not exist")
    if (plugin / "bin").exists():
        errs.append("plugin/bin/ must not exist")

    for base in (plugin, root / ".claude-plugin"):
        if not base.is_dir():
            continue
        for p in base.rglob("*.json"):
            data = load_json(p, errs)
            if data is not None and "permissions" in set(walk_keys(data)):
                errs.append(f"{p.relative_to(root)}: contains a 'permissions' key")

    for src in go_sources(root / "internal" / "hook"):
        text = src.read_text()
        for tok in HOOK_POLICY_TOKENS:
            if tok in text:
                errs.append(f"{src.relative_to(root)}: hook output must not contain {tok}")

    # The bundle renderer rewrites only launch commands in the plugin copy;
    # it must never introduce permission decisions or policy settings.
    for src in go_sources(root / "internal" / "bundle"):
        text = src.read_text()
        for tok in HOOK_POLICY_TOKENS + ['"permissions"', '"permission"', "allowedMcpServers", "deniedMcpServers", "enabled_tools", "disabled_tools"]:
            if tok in text:
                errs.append(f"{src.relative_to(root)}: the bundle renderer must not write {tok}")

    for src in go_sources(root / "internal" / "setup"):
        text = src.read_text()
        for tok in SETUP_POLICY_TOKENS:
            if tok in text:
                errs.append(f"{src.relative_to(root)}: setup must not write {tok}")
    return errs


def chosen_names(root, errs):
    """Yield (where, name) for every product/package/plugin/binary/
    marketplace/npm/command/skill/MCP name we choose."""
    mk = load_json(root / ".claude-plugin" / "marketplace.json", errs)
    if mk:
        yield ".claude-plugin/marketplace.json name", mk.get("name", "")
        for p in mk.get("plugins", []):
            yield ".claude-plugin/marketplace.json plugins[].name", p.get("name", "")

    plugin = root / "plugin"
    pj = load_json(plugin / ".claude-plugin" / "plugin.json", errs)
    if pj:
        yield "plugin.json name", pj.get("name", "")
    mcp = load_json(plugin / ".mcp.json", errs)
    if mcp:
        for server in (mcp.get("mcpServers") or {}):
            yield "plugin/.mcp.json server", server

    for p in sorted((plugin / "commands").glob("*.md")):
        yield "command", p.stem
    for p in sorted((plugin / "skills").glob("*/")):
        yield "skill", p.name
    for p in sorted((root / "cmd").glob("*/")):
        yield "binary", p.name

    for pkg in root.rglob("package.json"):
        if "node_modules" in pkg.parts:
            continue
        data = load_json(pkg, errs)
        if data and "name" in data:
            yield f"{pkg.relative_to(root)} name", data["name"]

    tool_name = re.compile(r'\bName:\s*"([^"]+)"')
    for src in go_sources(root / "internal" / "mcp"):
        for m in tool_name.finditer(src.read_text()):
            yield f"MCP tool ({src.relative_to(root)})", m.group(1)


# Build identity values (binary, plugin, marketplace and MCP server names,
# the shared root, keychain service, relay ports, environment variables,
# cookie names) live only in internal/variant, so that a build variant
# changes all of them together. Go code elsewhere reads them from
# variant.Current or variant.Shared. Tests, test/ and the plugin sources are
# exempt.
#
# The patterns are generated from internal/variant/variant.go: every
# variant table (Identity{...}) and the shared table (SharedValues{...}),
# so a value added to either is covered (issue #34 F3).
VARIANT_GO = pathlib.Path("internal") / "variant" / "variant.go"
TABLE = re.compile(r"^var (\w+) = (Identity|SharedValues)\{\n(.*?)^\}", re.S | re.M)
FIELD = re.compile(r"^\t(\w+):\s*(.+?),?\s*(?://.*)?$", re.M)


def variant_tables(root):
    """Return {table name: {field: value}} from variant.go (the repo's own
    when root has none): strings, string lists and int lists."""
    src = root / VARIANT_GO
    if not src.is_file():
        src = pathlib.Path(__file__).resolve().parent.parent / VARIANT_GO
    tables = {}
    for m in TABLE.finditer(src.read_text()):
        fields = {}
        for f in FIELD.finditer(m.group(3)):
            raw = f.group(2).strip()
            if raw.startswith('"'):
                fields[f.group(1)] = raw.strip('"')
            elif raw.startswith("[]string{"):
                fields[f.group(1)] = re.findall(r'"([^"]*)"', raw)
            elif raw.startswith("[]int{"):
                fields[f.group(1)] = [int(x) for x in re.findall(r"\d+", raw)]
        tables[m.group(1)] = fields
    return tables


def identity_literals(root):
    tables = variant_tables(root)
    ids = [t for n, t in tables.items() if "Binary" in t]
    shared = next((t for t in tables.values() if "KeychainService" in t), {})
    alt = lambda vals: "|".join(re.escape(v) for v in sorted(set(vals), key=len, reverse=True) if v)
    pats = []
    names = [i[k] for i in ids for k in ("Binary", "BundleName", "Marketplace", "Plugin", "MCPServer", "CaskToken") if i.get(k)]
    if names:
        pats.append((rf'"({alt(names)})(\.exe| )?"', "the executable, bundle, plugin, marketplace, MCP server or cask name"))
        pats.append((rf"Caskroom/({alt(names)})\b", "the Homebrew cask"))
    ids_ = [f"{i['Plugin']}@{i['Marketplace']}" for i in ids if i.get("Plugin") and i.get("Marketplace")]
    if ids_:
        pats.append((alt(ids_), "the plugin id"))
    disp = [i["DisplayName"] for i in ids if i.get("DisplayName")]
    if disp:
        pats.append((rf'"({alt(disp)}) approvals|Title:\s*"({alt(disp)})"', "the display name"))
    bins = [i["Binary"] for i in ids if i.get("Binary")]
    if bins:
        b = alt(bins)
        pats.append((rf"\.({b})-(backup|tmp)\b|\b({b})\.fish\b|>>> ({b}) PATH", "a PATH profile file, marker or backup name"))
    # Earlier releases' directories (the migration builds them from
    # variant.Kenaz.Binary).
    pats.append((r'kameas/kenaz(?![-\w])|"kameas",\s*"kenaz"', "an earlier release's config and state directory"))
    strs = [shared.get(k) for k in ("KeychainService", "RootDir", "HomeVar", "EnvVar", "MCPHostVar")]
    strs += [v for v in shared.get("EnvVars", []) if v.startswith("KENAZ_")]
    strs = [v for v in strs if v]
    if strs:
        pats.append((rf'"({alt(strs)})"', "the keychain service, the shared root or an environment variable name"))
    cookies = [shared.get(k) for k in ("ApprovalsCSRFCookie", "EnrollCookiePrefix") if shared.get(k)]
    if cookies:
        # Per-environment CSRF cookies insert the environment after the
        # first underscore.
        pats.append((rf"({alt(cookies)})|kenaz_\w+_approvals_csrf", "an approval page cookie name"))
    ports = [str(p) for p in shared.get("RelayPorts", []) if p]
    if ports:
        pats.append((rf"(?<![\w.])({alt(ports)})(?![\w.])", "a relay port"))
    return [(re.compile(p), what) for p, what in pats]


IDENTITY_EXEMPT_DIRS = ("internal/variant/", "test/", "plugin/")


def go_code(text):
    """Return text with Go comments blanked out (string and rune literals
    kept), so identity checks see only code."""
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if text.startswith("//", i):
            j = text.find("\n", i)
            i = n if j < 0 else j
            continue
        if text.startswith("/*", i):
            j = text.find("*/", i + 2)
            seg = text[i:n if j < 0 else j + 2]
            out.append("\n" * seg.count("\n"))
            i += len(seg)
            continue
        if c in "\"'`":
            j = i + 1
            while j < n and text[j] != c:
                if c != "`" and text[j] == "\\":
                    j += 1
                elif c != "`" and text[j] == "\n":
                    break
                j += 1
            out.append(text[i:j + 1])
            i = j + 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def check_identity_literals(root, errs):
    literals = identity_literals(root)
    for src in sorted(root.rglob("*.go")):
        rel = src.relative_to(root).as_posix()
        if src.name.endswith("_test.go") or rel.startswith(IDENTITY_EXEMPT_DIRS):
            continue
        if any(part in LEGACY_SKIP_DIRS for part in src.relative_to(root).parts):
            continue
        for n, line in enumerate(go_code(src.read_text()).splitlines(), 1):
            for pat, what in literals:
                m = pat.search(line)
                if m:
                    errs.append(f"{rel}:{n}: {m.group(0)!r} spells out {what}; use variant.Current or variant.Shared (internal/variant)")


def check_names(root):
    errs = []
    for where, name in chosen_names(root, errs):
        if name in EXEMPT_NAMES:
            continue
        if BANNED_NAME.search(name):
            errs.append(f"{where}: {name!r} contains a third-party mark (claude/codex/anthropic/openai)")
    check_identity_literals(root, errs)
    return errs


# The plugin was renamed to kenaz before its first release. The former name
# may remain only in these exact lines: values Kameas Fleet matches on or
# that feed a hash.
# Anything else must use the new name. Patterns are split so this file does
# not match itself.
LEGACY_NAME = re.compile("kameas" + r"[-_ ]?" + "fleet", re.IGNORECASE)
LEGACY_IGNORE_SPACED = re.compile("kameas" + " " + "fleet", re.IGNORECASE)  # "Kameas Fleet" is the service
LEGACY_ALLOW = {
    # Session-hash domain-separation label: changing it changes every hash.
    ("app/telemetry.go", 'h.Write([]byte("kameas-' + 'fleet/session-hash/v1\\x00"))'),
    # OTel service.name and instrumentation scope sent to Fleet. Fleet's
    # cold-export allowlist matches service.name by value.
    ("internal/project/encode.go", 'const ServiceName = "kameas-' + 'fleet"'),
    ("internal/project/encode.go", 'const ScopeName = "kameas-' + 'fleet/projector"'),
}
LEGACY_SKIP_DIRS = {".git", "bin", "dist", "node_modules"}
LEGACY_SKIP_FILES = {"go.sum"}


def check_legacy_name(root):
    errs = []
    allow = {}
    for path, line in LEGACY_ALLOW:
        allow.setdefault(path, set()).add(line)
    for p in sorted(root.rglob("*")):
        rel = p.relative_to(root)
        if any(part in LEGACY_SKIP_DIRS for part in rel.parts) or not p.is_file() or p.name in LEGACY_SKIP_FILES:
            continue
        try:
            text = p.read_text()
        except (UnicodeDecodeError, OSError):
            continue
        key = rel.as_posix()
        if LEGACY_NAME.search(key):
            errs.append(f"{key}: path uses the former name; use kenaz")
        for n, line in enumerate(text.splitlines(), 1):
            hits = [m for m in LEGACY_NAME.finditer(line) if not LEGACY_IGNORE_SPACED.fullmatch(m.group(0))]
            if not hits:
                continue
            if any(a in line for a in allow.get(key, ())):
                continue
            errs.append(f"{key}:{n}: former name {hits[0].group(0)!r}; use kenaz (or KENAZ_ for env vars)")
    return errs


def main(argv):
    checks = {"no-policy": check_no_policy, "names": check_names, "legacy-name": check_legacy_name}
    if len(argv) < 2 or argv[1] not in checks:
        print(__doc__, file=sys.stderr)
        return 64
    root = pathlib.Path(argv[2] if len(argv) > 2 else ".").resolve()
    errs = checks[argv[1]](root)
    for e in errs:
        print(f"check-{argv[1]}: {e}", file=sys.stderr)
    if not errs:
        print(f"check-{argv[1]}: ok")
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
