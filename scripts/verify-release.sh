#!/usr/bin/env bash
# verify-release.sh (--checksums <checksums.txt> [--name <asset>] | --sha256 <hex>) <official-binary> <public-tag>
#
# Checks an official kenaz binary against this public repository.
# Run by the official release workflow; anyone can run it
# against a downloaded release.
#
#  0. The binary is copied into a private 0700 temporary directory; every
#     step below uses that copy, never the original path again.
#  1. Integrity first: the executable's SHA-256 must equal the published
#     digest (--checksums, with --name selecting the entry, default the
#     binary's file name; or --sha256). On a mismatch the script stops: the
#     binary is never run. Without a digest nothing is run either: the
#     static module checks below still run, and the result is UNVERIFIED
#     (exit 3), never "ok".
#  2. The module list is read statically with `go version -m` (Go must be
#     installed; $GO overrides the command), never from the binary's own
#     output. It holds exactly one public module entry, at exactly
#     <public-tag> (not a prefix such as v0.1.01), not a fork and not
#     replaced, and exactly one other github.com/kameas-ai/ module (the
#     proprietary one), unreplaced.
#  3. Only then is `version --json` run: the official flavor and
#     non-placeholder provenance, version <public-tag> without the "v",
#     public_commit the full commit SHA <public-tag> points at in this repo,
#     public_module_version exactly <public-tag>, and private_module_version
#     equal to the proprietary module's version in the module list.
#
# Paths may be relative to the caller's working directory or absolute;
# the script never changes directory. The private copy lives under $TMPDIR
# (mktemp -d), which must allow running programs: on a noexec TMPDIR, set
# TMPDIR to another directory.
#
# Exit status: 0 verified, 1 a check failed, 2 usage, 3 UNVERIFIED (no
# digest given).
#
# What it cannot prove: the proprietary module's behavior. That is covered
# by the official build's allowlist CI gate and documented in the
# release notes.
set -euo pipefail
usage="usage: verify-release.sh (--checksums <file> [--name <asset>] | --sha256 <hex>) <binary> <public-tag>"
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

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "verify-release: tag must be vX.Y.Z[-pre], got '$tag'" >&2; exit 1; }
want_commit="$(git -C "$root" rev-parse --verify --quiet "refs/tags/$tag^{commit}")" \
  || { echo "verify-release: tag $tag not found in $root" >&2; exit 1; }

PYTHON="$(command -v python3 || command -v python || true)"
[ -n "$PYTHON" ] || { echo "verify-release: python3 is required" >&2; exit 1; }

# Work on a private copy: it is hashed, inspected and run from a 0700
# directory nobody else can write, so the file checked is the file run
# (the original path is never reopened).
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
copy="$work/$(basename "$bin")"
cp "$bin" "$copy"
chmod 700 "$copy"
[ -n "$name" ] || name="$(basename "$bin")"

"$PYTHON" - "$tag" "$want_commit" "$copy" "$checksums" "$name" "$want_sha" "${GO:-go}" <<'PY'
import hashlib, json, os, re, subprocess, sys

tag, want, binpath, checksums, name, want_sha, gocmd = sys.argv[1:8]
PUBLIC = "github.com/kameas-ai/kenaz-plugin"
fail = []

def finish(status_line=None):
    if fail:
        print("verify-release: FAIL")
        for f in fail:
            print("  -", f)
        sys.exit(1)
    if status_line:
        print(status_line)

# 1. Integrity, before anything reads or runs the binary's contents.
h = hashlib.sha256()
with open(binpath, "rb") as fh:
    for chunk in iter(lambda: fh.read(1 << 20), b""):
        h.update(chunk)
got = h.hexdigest()
verified = False
if checksums:
    asset = name or os.path.basename(binpath)
    try:
        with open(checksums, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError as e:
        fail.append(f"cannot read checksums: {e}")
        finish()
    hits = [l.split()[0].lower() for l in lines if len(l.split()) == 2 and l.split()[1].lstrip("*") == asset]
    if len(hits) != 1:
        fail.append(f"checksums file has {len(hits)} entries for {asset}, want exactly 1")
        finish()
    want_sha = hits[0]
if want_sha:
    if not re.fullmatch(r"[0-9a-f]{64}", want_sha.lower()):
        fail.append(f"expected SHA-256 {want_sha!r} is malformed")
    elif got != want_sha.lower():
        fail.append(f"executable SHA-256 {got} != published {want_sha.lower()} (modified executable; it was not run)")
    else:
        verified = True
    finish()

# 2. The module list, read statically by the Go toolchain.
try:
    out = subprocess.run([gocmd, "version", "-m", binpath], capture_output=True, text=True, check=True).stdout
except FileNotFoundError:
    fail.append(f"{gocmd!r} not found: Go is needed to read the module list (go version -m)")
    finish()
except subprocess.CalledProcessError as e:
    fail.append(f"go version -m could not read the binary's build info: {e.stderr.strip()}")
    finish()
deps = []  # [path, version, replacement or ""]
for line in out.splitlines():
    f = line.strip().split("\t")
    if len(f) >= 3 and f[0] == "dep":
        deps.append([f[1], f[2], ""])
    elif len(f) >= 2 and f[0] == "=>" and deps:
        deps[-1][2] = " ".join(f[1:3])

pub = [d for d in deps if d[0] == PUBLIC]
if len(pub) != 1:
    fail.append(f"binary lists {len(pub)} {PUBLIC} module entries, want exactly 1")
elif pub[0][2]:
    fail.append(f"public module is replaced in the official build (=> {pub[0][2]})")
elif pub[0][1] != tag:
    fail.append(f"public module version in binary is {pub[0][1]!r}, want exactly {tag}")
priv = [d for d in deps if d[0].startswith("github.com/kameas-ai/") and d[0] != PUBLIC]
if len(priv) != 1:
    fail.append(f"binary lists {len(priv)} other github.com/kameas-ai/ modules, want exactly 1 (the private module)")
elif priv[0][2]:
    fail.append(f"private module is replaced in the official build (=> {priv[0][2]})")
finish()

if not verified:
    print(f"verify-release: UNVERIFIED — module list matches {tag}, but no published digest was given "
          "(pass --checksums or --sha256), so the binary was not run and its provenance is not verified")
    sys.exit(3)

# 3. Provenance from the verified binary.
try:
    raw = subprocess.run([binpath, "version", "--json"], capture_output=True, text=True, check=True, timeout=60).stdout
    p = json.loads(raw)
except PermissionError as e:
    fail.append(f"cannot run the private copy in {os.path.dirname(binpath)} ({e.strerror}); its temporary "
                "directory is probably on a noexec filesystem: set TMPDIR to a directory where programs "
                "can run (for example TMPDIR=$HOME/tmp) and try again")
    finish()
except (OSError, subprocess.SubprocessError, ValueError) as e:
    fail.append(f"version --json failed: {e}")
    finish()

if p.get("flavor") != "official":
    fail.append(f"flavor is {p.get('flavor')!r}, want 'official'")
for k in ("version", "public_commit", "public_module_version", "private_module_version", "private_commit", "build_date"):
    if p.get(k) in (None, "", "unknown"):
        fail.append(f"{k} is a placeholder")
if p.get("version") not in (None, "", "unknown") and p.get("version") != tag[1:]:
    fail.append(f"version {p.get('version')} != {tag[1:]}")
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
if priv[0][1] != p.get("private_module_version"):
    fail.append(f"private module version in binary is {priv[0][1]!r}, but version --json reports {p.get('private_module_version')!r}")
finish(f"verify-release: ok — {p['version']} built from public {tag} ({p['public_commit']}) + private {p['private_module_version']}; sha256 {got}")
PY
