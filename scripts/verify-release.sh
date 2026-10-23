#!/usr/bin/env bash
# verify-release.sh [--checksums <checksums.txt> [--name <asset>] | --sha256 <hex>] <official-binary> <public-tag>
#
# Checks an official kenaz binary against this public repository.
# Run by the official release workflow; anyone can run it
# against a downloaded release.
#
#  1. `version --json` reports the official flavor and non-placeholder
#     provenance, and its version is <public-tag> without the leading "v".
#  2. public_commit is the full commit SHA <public-tag> points at in this
#     repo (no abbreviations, no module versions), and
#     public_module_version is exactly <public-tag>.
#  3. The binary's module list holds exactly one public module entry, at
#     exactly <public-tag> (not a prefix such as v0.1.01), and not a fork or
#     a replace. Exactly one other github.com/kameas-ai/ module (the
#     proprietary one) is listed, unreplaced, at the private_module_version
#     the binary reports.
#  4. With --checksums or --sha256: the executable's SHA-256 equals the
#     published digest, so a modified executable is rejected. --name selects
#     the checksums.txt entry (default: the binary's file name).
#
# <official-binary> may be relative to the caller's working directory or
# absolute; the script never changes directory.
#
# What it cannot prove: the proprietary module's behavior. That is covered
# by the official build's allowlist CI gate and documented in the
# release notes.
set -euo pipefail
usage="usage: verify-release.sh [--checksums <file> [--name <asset>] | --sha256 <hex>] <binary> <public-tag>"
checksums="" name="" want_sha=""
while [ $# -gt 0 ]; do
  case "$1" in
    --checksums) checksums="${2:?$usage}"; shift 2 ;;
    --name) name="${2:?$usage}"; shift 2 ;;
    --sha256) want_sha="${2:?$usage}"; shift 2 ;;
    --) shift; break ;;
    -*) echo "$usage" >&2; exit 2 ;;
    *) break ;;
  esac
done
[ $# -eq 2 ] || { echo "$usage" >&2; exit 2; }
bin="$1" tag="$2"
[ -z "$checksums" ] || [ -z "$want_sha" ] || { echo "verify-release: pass --checksums or --sha256, not both" >&2; exit 2; }

abs() {
  case "$1" in
    /*|[A-Za-z]:[\\/]*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "$PWD" "$1" ;;
  esac
}
# Resolve caller-relative paths up front: nothing below depends on the cwd.
bin="$(abs "$bin")"
[ -z "$checksums" ] || checksums="$(abs "$checksums")"
[ -f "$bin" ] || { echo "verify-release: binary not found: $bin" >&2; exit 1; }
[ -x "$bin" ] || { echo "verify-release: binary is not executable: $bin" >&2; exit 1; }

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "verify-release: tag must be vX.Y.Z[-pre], got '$tag'" >&2; exit 1; }
want_commit="$(git -C "$root" rev-parse --verify --quiet "refs/tags/$tag^{commit}")" \
  || { echo "verify-release: tag $tag not found in $root" >&2; exit 1; }

PYTHON="$(command -v python3 || command -v python || true)"
[ -n "$PYTHON" ] || { echo "verify-release: python3 is required" >&2; exit 1; }
json="$("$bin" version --json)" || { echo "verify-release: $bin version --json failed" >&2; exit 1; }

"$PYTHON" - "$json" "$tag" "$want_commit" "$bin" "$checksums" "$name" "$want_sha" <<'PY'
import hashlib, json, os, re, sys

raw, tag, want, binpath, checksums, name, want_sha = sys.argv[1:8]
fail = []
try:
    p = json.loads(raw)
except ValueError as e:
    print(f"verify-release: FAIL\n  - version --json is not JSON: {e}")
    sys.exit(1)

if p.get("flavor") != "official":
    fail.append(f"flavor is {p.get('flavor')!r}, want 'official'")
for k in ("version", "public_commit", "public_module_version", "private_module_version", "private_commit", "build_date"):
    if p.get(k) in (None, "", "unknown"):
        fail.append(f"{k} is a placeholder")
if p.get("version") not in (None, "", "unknown") and p.get("version") != tag[1:]:
    fail.append(f"version {p.get('version')} != {tag[1:]}")

# Provenance names the full commit the tag points at. A module version, an
# abbreviation or any other commit is rejected.
pc = p.get("public_commit") or ""
if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", pc):
    fail.append(f"public_commit {pc!r} is not a full commit SHA")
elif pc != want:
    fail.append(f"public_commit {pc} != {tag} ({want})")
if p.get("public_module_version") != tag:
    fail.append(f"public_module_version {p.get('public_module_version')} != {tag}")
pc2 = p.get("private_commit") or ""
if pc2 not in ("", "unknown") and not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", pc2):
    fail.append(f"private_commit {pc2!r} is not a full commit SHA")

def entries(path):
    out = []
    for m in p.get("modules") or []:
        parts = m.split(" ", 1)
        if parts[0] == path:
            out.append(parts[1] if len(parts) > 1 else "")
    return out

pub = entries("github.com/kameas-ai/kenaz-plugin")
if len(pub) != 1:
    fail.append(f"binary lists {len(pub)} github.com/kameas-ai/kenaz-plugin module entries, want exactly 1")
else:
    v = pub[0]
    if "=>" in v:
        fail.append(f"public module is replaced in the official build ({v})")
    elif v != tag:
        fail.append(f"public module version in binary is {v!r}, want exactly {tag}")

# The proprietary module: the only other github.com/kameas-ai/ module.
priv = [m for m in p.get("modules") or []
        if m.startswith("github.com/kameas-ai/") and m.split(" ", 1)[0] != "github.com/kameas-ai/kenaz-plugin"]
if len(priv) != 1:
    fail.append(f"binary lists {len(priv)} other github.com/kameas-ai/ modules, want exactly 1 (the private module)")
else:
    v = priv[0].split(" ", 1)[1] if " " in priv[0] else ""
    if "=>" in v:
        fail.append(f"private module is replaced in the official build ({v})")
    elif v != p.get("private_module_version"):
        fail.append(f"private module version in binary is {v!r}, but version --json reports {p.get('private_module_version')!r}")

digest_note = "digest not checked (pass --checksums or --sha256)"
if checksums or want_sha:
    h = hashlib.sha256()
    with open(binpath, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    got = h.hexdigest()
    if checksums:
        asset = name or os.path.basename(binpath)
        try:
            with open(checksums, encoding="utf-8") as fh:
                lines = fh.read().splitlines()
        except OSError as e:
            lines = None
            fail.append(f"cannot read checksums: {e}")
        if lines is not None:
            hits = [l.split()[0].lower() for l in lines if len(l.split()) == 2 and l.split()[1].lstrip("*") == asset]
            if len(hits) != 1:
                fail.append(f"checksums file has {len(hits)} entries for {asset}, want exactly 1")
            else:
                want_sha = hits[0]
    if want_sha:
        if not re.fullmatch(r"[0-9a-f]{64}", want_sha.lower()):
            fail.append(f"expected SHA-256 {want_sha!r} is malformed")
        elif got != want_sha.lower():
            fail.append(f"executable SHA-256 {got} != published {want_sha.lower()} (modified executable)")
        else:
            digest_note = f"sha256 {got}"

if fail:
    print("verify-release: FAIL")
    for f in fail:
        print("  -", f)
    sys.exit(1)
print(f"verify-release: ok — {p['version']} built from public {tag} ({p['public_commit']}) + private {p['private_module_version']}; {digest_note}")
PY
