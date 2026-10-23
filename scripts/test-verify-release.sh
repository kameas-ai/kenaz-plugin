#!/usr/bin/env bash
# Regression tests for scripts/verify-release.sh (release provenance and
# integrity). Builds an isolated tagged repository in a path with spaces, a fake
# official binary that prints a chosen `version --json`, and checks that the
# verifier accepts the genuine build by relative and absolute path and rejects
# every provenance or integrity defect.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

repo="$tmp/public repo"
mkdir -p "$repo/scripts"
cp "$here/scripts/verify-release.sh" "$repo/scripts/"
g() { git -C "$repo" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }
g init -q
g add . && g commit -qm one
other="$(g rev-parse HEAD)"
echo x > "$repo/x" && g add x && g commit -qm two
g tag -a v0.1.0 -m v0.1.0
want="$(g rev-parse 'v0.1.0^{commit}')"
priv=0123456789abcdef0123456789abcdef01234567
verify="$repo/scripts/verify-release.sh"

bindir="$tmp/bin dir"
mkdir -p "$bindir"
fake="$bindir/kenaz"
# write_bin <json-python-expr-overrides>: a fake binary printing version --json.
write_bin() {
  python3 - "$fake" "$want" "$priv" "$@" <<'PY'
import json, sys
path, want, priv = sys.argv[1:4]
p = {
    "version": "0.1.0", "flavor": "official", "public_commit": want,
    "public_module_version": "v0.1.0", "private_module_version": "v0.2.0",
    "private_commit": priv, "build_date": "2026-10-23T00:00:00Z", "go_version": "go1.27.3",
    "modules": ["github.com/kameas-ai/private-client v0.2.0", "github.com/kameas-ai/kenaz-plugin v0.1.0", "golang.org/x/sys v0.47.0"],
}
for kv in sys.argv[4:]:
    k, v = kv.split("=", 1)
    p[k] = json.loads(v)
with open(path, "w") as fh:
    fh.write("#!/bin/sh\ncat <<'EOF'\n" + json.dumps(p) + "\nEOF\n")
PY
  chmod +x "$fake"
}
sums() { (cd "$bindir" && sha256sum kenaz > "$tmp/checksums.txt"); }

pass() {
  local label="$1"; shift
  if ! out="$("$@" 2>&1)"; then echo "FAIL: $label should pass:" >&2; echo "$out" >&2; exit 1; fi
  echo "ok: $label"
}
reject() {
  local label="$1" expect="$2"; shift 2
  if out="$("$@" 2>&1)"; then echo "FAIL: $label was accepted:" >&2; echo "$out" >&2; exit 1; fi
  grep -qF -- "$expect" <<<"$out" || { echo "FAIL: $label rejected for the wrong reason (want '$expect'):" >&2; echo "$out" >&2; exit 1; }
  echo "ok: rejects $label"
}

write_bin; sums
pass "absolute binary path" "$verify" "$fake" v0.1.0
# Relative path from an unrelated cwd: the script must not cd before running it.
pass "relative binary path" bash -c 'cd "$1" && "$2" "bin dir/kenaz" v0.1.0' _ "$tmp" "$verify"
pass "relative ./ binary path" bash -c 'cd "$1" && "$2" ./kenaz v0.1.0' _ "$bindir" "$verify"
pass "relative checksums + binary" bash -c 'cd "$1" && "$2" --checksums checksums.txt "bin dir/kenaz" v0.1.0' _ "$tmp" "$verify"
pass "explicit --sha256" "$verify" --sha256 "$(cut -d' ' -f1 "$tmp/checksums.txt")" "$fake" v0.1.0

# Missing public module.
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0"]'
reject "missing public module" "0 github.com/kameas-ai/kenaz-plugin module entries" "$verify" "$fake" v0.1.0
# Prefix-colliding version: v0.1.01 must not satisfy v0.1.0.
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.01"]'
reject "module v0.1.01 for tag v0.1.0" "want exactly v0.1.0" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0-rc1"]'
reject "module v0.1.0-rc1 for tag v0.1.0" "want exactly v0.1.0" "$verify" "$fake" v0.1.0
write_bin 'public_module_version="v0.1.01"'
reject "public_module_version v0.1.01" "public_module_version v0.1.01 != v0.1.0" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "duplicate public module" "2 github.com/kameas-ai/kenaz-plugin module entries" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0 => ../fork "]'
reject "replaced public module" "public module is replaced" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "missing private module" "0 other github.com/kameas-ai/ modules" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.1.9","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "private module version mismatch" "private module version in binary is 'v0.1.9'" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin-fork v0.1.0"]'
reject "look-alike public module path" "0 github.com/kameas-ai/kenaz-plugin module entries" "$verify" "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/extra v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "second private module" "2 other github.com/kameas-ai/ modules" "$verify" "$fake" v0.1.0

# Wrong commit: another commit, an abbreviation, a module version.
write_bin "public_commit=\"$other\""
reject "wrong public commit" "public_commit $other != v0.1.0" "$verify" "$fake" v0.1.0
write_bin "public_commit=\"${want:0:12}\""
reject "abbreviated public commit" "is not a full commit SHA" "$verify" "$fake" v0.1.0
write_bin 'public_commit="v0.1.0"'
reject "module version as public commit" "is not a full commit SHA" "$verify" "$fake" v0.1.0
write_bin 'private_commit="v0.2.0"'
reject "module version as private commit" "private_commit 'v0.2.0' is not a full commit SHA" "$verify" "$fake" v0.1.0

# Flavor, placeholders, version and tag.
write_bin 'flavor="unofficial"'
reject "unofficial flavor" "flavor is 'unofficial'" "$verify" "$fake" v0.1.0
write_bin 'build_date="unknown"'
reject "placeholder build date" "build_date is a placeholder" "$verify" "$fake" v0.1.0
write_bin 'version="0.1.1"'
reject "binary version mismatch" "version 0.1.1 != 0.1.0" "$verify" "$fake" v0.1.0
write_bin
reject "unknown tag" "tag v9.9.9 not found" "$verify" "$fake" v9.9.9
reject "non-semver tag" "tag must be vX.Y.Z" "$verify" "$fake" main
reject "missing binary" "binary not found" "$verify" "$bindir/nope" v0.1.0

# Modified executable: digest no longer matches the published checksums.
write_bin; sums
printf '# tampered\n' >> "$fake"
reject "modified executable (--checksums)" "(modified executable)" "$verify" --checksums "$tmp/checksums.txt" "$fake" v0.1.0
reject "modified executable (--sha256)" "(modified executable)" "$verify" --sha256 "$(cut -d' ' -f1 "$tmp/checksums.txt")" "$fake" v0.1.0
reject "asset absent from checksums" "0 entries for other" "$verify" --checksums "$tmp/checksums.txt" --name other "$fake" v0.1.0

echo "test-verify-release: ok"
