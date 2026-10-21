#!/usr/bin/env bash
# verify-release.sh <official-binary> <public-tag>
#
# Checks an official kenaz binary against this public repository.
# Run by the official release workflow; anyone can run it
# against a downloaded release.
#
#  1. `version --json` reports the official flavor and non-placeholder
#     provenance.
#  2. public_commit is the commit <public-tag> points at in this repo, and
#     public_module_version is <public-tag>.
#  3. The public module in the binary's module list is this repo at that
#     version (not a fork or a replace).
#
# What it cannot prove: the proprietary module's behavior. That is covered
# by the official build's allowlist CI gate and documented in the
# release notes.
set -euo pipefail
bin="${1:?usage: verify-release.sh <binary> <public-tag>}"
tag="${2:?usage: verify-release.sh <binary> <public-tag>}"
cd "$(dirname "$0")/.."

want_commit="$(git rev-list -n1 "$tag" 2>/dev/null)" || { echo "verify-release: tag $tag not found in this repo" >&2; exit 1; }
json="$("$bin" version --json)"

python3 - "$json" "$tag" "$want_commit" <<'PY'
import json, sys
p, tag, want = json.loads(sys.argv[1]), sys.argv[2], sys.argv[3]
fail = []
if p.get("flavor") != "official":
    fail.append(f"flavor is {p.get('flavor')!r}, want 'official'")
for k in ("public_commit", "public_module_version", "private_module_version", "private_commit", "build_date"):
    if p.get(k) in (None, "", "unknown"):
        fail.append(f"{k} is a placeholder")
if p.get("public_commit") not in (want, want[:12]) and not want.startswith(p.get("public_commit") or "-"):
    fail.append(f"public_commit {p.get('public_commit')} != {tag} ({want})")
if p.get("public_module_version") != tag:
    fail.append(f"public_module_version {p.get('public_module_version')} != {tag}")
mods = p.get("modules") or []
pub = [m for m in mods if m.startswith("github.com/kameas-ai/kenaz-plugin ")]
if any("=>" in m for m in pub):
    fail.append("public module is replaced in the official build")
if pub and not any(m.split(" ", 1)[1].startswith(tag) for m in pub):
    fail.append(f"public module version in binary is {pub[0].split(' ', 1)[1]}, want {tag}")
if fail:
    print("verify-release: FAIL")
    for f in fail:
        print("  -", f)
    sys.exit(1)
print(f"verify-release: ok — {p['version']} built from public {tag} ({p['public_commit']}) + private {p['private_module_version']}")
PY
