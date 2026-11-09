#!/usr/bin/env python3
"""Assemble a self-contained bundle around a staff build of the program, so that
`kenaz setup claude --bundle DIR` can register it (internal/bundle.LoadBundle). It is
for staff trying a build that no release carries: docs/memory-staff.md.

usage: scripts/staff-bundle.py --binary PATH --out DIR [--suffix TEXT]

The plugin's version gets a suffix so that a host treats it as a new plugin and does
not reuse a cached copy of the released one. Nothing is signed or published.

staff/plugin/ is laid over plugin/: what staff try before customers get it, which a
customer build must not carry. plugin/ is what customers install. So are the Codex
prompts for tools only a staff build serves, which are added to the copy.
"""
import argparse
import hashlib
import json
import pathlib
import platform
import shutil
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
GOOS = {"darwin": "darwin", "linux": "linux"}
GOARCH = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "amd64": "amd64"}
VERSIONED = ["plugin/.claude-plugin/plugin.json", "plugin/.codex-plugin/plugin.json"]
MARKETPLACE = ".claude-plugin/marketplace.json"
CODEX_MCP = "plugin/.codex-mcp.json"
# Tools only a staff build serves that Codex must ask about on every call.
STAFF_CODEX_PROMPT_TOOLS = ["context_share_file"]


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--binary", required=True, type=pathlib.Path)
    ap.add_argument("--out", required=True, type=pathlib.Path)
    ap.add_argument("--suffix", default="staff")
    args = ap.parse_args()
    if args.out.exists():
        sys.exit(f"{args.out} already exists: choose a new directory")
    goos = GOOS.get(sys.platform)
    goarch = GOARCH.get(platform.machine().lower())
    if not goos or not goarch:
        sys.exit("capture is not available on this platform")

    shutil.copytree(ROOT / "plugin", args.out / "plugin")
    shutil.copytree(ROOT / "staff" / "plugin", args.out / "plugin", dirs_exist_ok=True)
    p = args.out / CODEX_MCP
    d = json.loads(p.read_text())
    for tool in STAFF_CODEX_PROMPT_TOOLS:
        d["mcpServers"]["kenaz"]["tools"][tool] = {"approval_mode": "prompt"}
    p.write_text(json.dumps(d, indent=2) + "\n")
    (args.out / ".claude-plugin").mkdir(parents=True)
    shutil.copyfile(ROOT / MARKETPLACE, args.out / MARKETPLACE)
    (args.out / "bin").mkdir()
    exe = args.out / "bin" / "kenaz"
    shutil.copyfile(args.binary, exe)
    exe.chmod(0o755)

    version = ""
    for rel in VERSIONED:
        p = args.out / rel
        d = json.loads(p.read_text())
        version = f"{d['version']}-{args.suffix}"
        d["version"] = version
        p.write_text(json.dumps(d, indent=2) + "\n")
    p = args.out / MARKETPLACE
    d = json.loads(p.read_text())
    for plugin in d["plugins"]:
        plugin["version"] = version
    p.write_text(json.dumps(d, indent=2) + "\n")

    files = {}
    for path in sorted(args.out.rglob("*")):
        if path.is_file():
            files[path.relative_to(args.out).as_posix()] = sha256(path)
    manifest = {
        "schema": 1,
        "name": "kenaz",
        "version": version,
        "os": goos,
        "arch": goarch,
        "executable": "bin/kenaz",
        "executable_sha256": files["bin/kenaz"],
        "opencode": False,
        "files": files,
    }
    (args.out / "bundle.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(args.out)


if __name__ == "__main__":
    main()
