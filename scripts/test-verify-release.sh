#!/usr/bin/env bash
# Regression tests for scripts/verify-release.sh (release provenance and
# integrity). Builds an isolated tagged repository in a path with spaces, a fake
# official binary that prints a chosen `version --json`, a fake `go` whose
# `go version -m` prints a chosen module list, and checks that the verifier
# accepts the genuine build by relative and absolute path, never runs an
# unverified binary, and rejects every provenance or integrity defect.
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
ran="$tmp/ran"
gomods="$tmp/gomods"
# A fake `go` whose `go version -m` prints the module list write_bin chose
# (the real toolchain reads it from the binary's build info).
fakego="$tmp/fake go"
cat > "$fakego" <<SH
#!/bin/sh
[ "\$1 \$2" = "version -m" ] || exit 2
printf '%s: go1.27.3\n' "\$3"
cat "$gomods"
SH
chmod +x "$fakego"
export GO="$fakego"

# write_bin [key=json ...]: a fake binary printing version --json (and
# touching $ran when run), its `go version -m` module list, and the
# published checksums for it. modules=[...] entries are "path version
# [=> replacement version]".
write_bin() {
  python3 - "$fake" "$want" "$priv" "$ran" "$gomods" "$@" <<'PY'
import json, sys
path, want, priv, ran, gomods = sys.argv[1:6]
p = {
    "version": "0.1.0", "flavor": "official", "public_commit": want,
    "public_module_version": "v0.1.0", "private_module_version": "v0.2.0",
    "private_commit": priv, "build_date": "2026-10-23T00:00:00Z", "go_version": "go1.27.3",
    "modules": ["github.com/kameas-ai/private-client v0.2.0", "github.com/kameas-ai/kenaz-plugin v0.1.0", "golang.org/x/sys v0.47.0"],
}
claimed = None
for kv in sys.argv[6:]:
    k, v = kv.split("=", 1)
    if k == "claimed_modules":  # what version --json says (ignored by the verifier)
        claimed = json.loads(v)
        continue
    p[k] = json.loads(v)
mods = p["modules"]
if claimed is not None:
    p["modules"] = claimed
with open(gomods, "w") as fh:
    fh.write("\tpath\tgithub.com/kameas-ai/kenaz-release/cmd/kenaz\n\tmod\tgithub.com/kameas-ai/kenaz-release\t(devel)\t\n")
    for m in mods:
        left, _, repl = m.partition(" => ")
        mp, mv = (left.split(" ", 1) + [""])[:2]
        fh.write(f"\tdep\t{mp}\t{mv}\th1:x=\n")
        if repl.strip():
            rp, rv = (repl.strip().split(" ", 1) + [""])[:2]
            fh.write(f"\t=>\t{rp}\t{rv}\t\n")
with open(path, "w") as fh:
    fh.write("#!/bin/sh\nprintf '%s' \"$0\" > '" + ran + "'\ncat <<'EOF'\n" + json.dumps(p) + "\nEOF\n")
PY
  chmod +x "$fake"
  rm -f "$ran"
  (cd "$bindir" && sha256sum kenaz > "$tmp/checksums.txt")
}

pass() {
  local label="$1"; shift
  if ! out="$("$@" 2>&1)"; then echo "FAIL: $label should pass:" >&2; echo "$out" >&2; exit 1; fi
  grep -q "verify-release: ok" <<<"$out" || { echo "FAIL: $label did not report ok:" >&2; echo "$out" >&2; exit 1; }
  echo "ok: $label"
}
reject() {
  local label="$1" expect="$2"; shift 2
  if out="$("$@" 2>&1)"; then echo "FAIL: $label was accepted:" >&2; echo "$out" >&2; exit 1; fi
  grep -qF -- "$expect" <<<"$out" || { echo "FAIL: $label rejected for the wrong reason (want '$expect'):" >&2; echo "$out" >&2; exit 1; }
  echo "ok: rejects $label"
}
# v: the verifier with the published checksums.
v() { "$verify" --checksums "$tmp/checksums.txt" "$@"; }

write_bin
pass "absolute binary path" v "$fake" v0.1.0
# Relative paths from an unrelated cwd: the script must not cd first.
pass "relative binary path" bash -c 'cd "$1" && "$2" --checksums checksums.txt "bin dir/kenaz" v0.1.0' _ "$tmp" "$verify"
pass "relative ./ binary path" bash -c 'cd "$1" && "$2" --checksums ../checksums.txt ./kenaz v0.1.0' _ "$bindir" "$verify"
pass "explicit --sha256" "$verify" --sha256 "$(cut -d' ' -f1 "$tmp/checksums.txt")" "$fake" v0.1.0
# The binary run is a private copy, never the original path.
ranpath="$(cat "$ran")"
[ "$ranpath" != "$fake" ] && [ "$(basename "$ranpath")" = kenaz ] && [ ! -e "$ranpath" ] \
  || { echo "FAIL: ran $ranpath, want a private copy (removed afterwards)" >&2; exit 1; }
echo "ok: runs a private copy and removes it"

# Without a digest: never "ok", exit 3, and the binary is not run.
rm -f "$ran"
set +e; out="$("$verify" "$fake" v0.1.0 2>&1)"; st=$?; set -e
[ "$st" -eq 3 ] && grep -q "UNVERIFIED" <<<"$out" && ! grep -q "verify-release: ok" <<<"$out" \
  || { echo "FAIL: no digest should be UNVERIFIED (exit 3), got $st:" >&2; echo "$out" >&2; exit 1; }
[ ! -e "$ran" ] || { echo "FAIL: the binary was run without a digest" >&2; exit 1; }
echo "ok: no digest is UNVERIFIED and runs nothing"

# Missing public module.
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0"]'
reject "missing public module" "0 github.com/kameas-ai/kenaz-plugin module entries" v "$fake" v0.1.0
# Prefix-colliding version: v0.1.01 must not satisfy v0.1.0.
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.01"]'
reject "module v0.1.01 for tag v0.1.0" "want exactly v0.1.0" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0-rc1"]'
reject "module v0.1.0-rc1 for tag v0.1.0" "want exactly v0.1.0" v "$fake" v0.1.0
write_bin 'public_module_version="v0.1.01"'
reject "public_module_version v0.1.01" "public_module_version v0.1.01 != v0.1.0" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "duplicate public module" "2 github.com/kameas-ai/kenaz-plugin module entries" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0 => ../fork"]'
reject "replaced public module" "public module is replaced" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "missing private module" "0 other github.com/kameas-ai/ modules" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.1.9","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "private module version mismatch" "private module version in binary is 'v0.1.9'" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0 => ../priv-fork","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "replaced private module" "private module is replaced" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin-fork v0.1.0"]'
reject "look-alike public module path" "0 github.com/kameas-ai/kenaz-plugin module entries" v "$fake" v0.1.0
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/extra v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "second private module" "2 other github.com/kameas-ai/ modules" v "$fake" v0.1.0
# The module list comes from go version -m, not from what the binary says.
write_bin 'modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0 => ../fork"]' \
  'claimed_modules=["github.com/kameas-ai/private-client v0.2.0","github.com/kameas-ai/kenaz-plugin v0.1.0"]'
reject "fork hidden from version --json" "public module is replaced" v "$fake" v0.1.0

# Wrong commit: another commit, an abbreviation, a module version.
write_bin "public_commit=\"$other\""
reject "wrong public commit" "public_commit $other != v0.1.0" v "$fake" v0.1.0
write_bin "public_commit=\"${want:0:12}\""
reject "abbreviated public commit" "is not a full commit SHA" v "$fake" v0.1.0
write_bin 'public_commit="v0.1.0"'
reject "module version as public commit" "is not a full commit SHA" v "$fake" v0.1.0
write_bin 'private_commit="v0.2.0"'
reject "module version as private commit" "private_commit 'v0.2.0' is not a full commit SHA" v "$fake" v0.1.0

# Flavor, placeholders, version and tag.
write_bin 'flavor="unofficial"'
reject "unofficial flavor" "flavor is 'unofficial'" v "$fake" v0.1.0
write_bin 'build_date="unknown"'
reject "placeholder build date" "build_date is a placeholder" v "$fake" v0.1.0
write_bin 'version="0.1.1"'
reject "binary version mismatch" "version 0.1.1 != 0.1.0" v "$fake" v0.1.0
write_bin
reject "unknown tag" "tag v9.9.9 not found" v "$fake" v9.9.9
reject "non-semver tag" "tag must be vX.Y.Z" v "$fake" main
reject "missing binary" "binary not found" v "$bindir/nope" v0.1.0

# Modified executable: the digest no longer matches and it is never run.
write_bin
printf '# tampered\n' >> "$fake"
rm -f "$ran"
reject "modified executable (--checksums)" "(modified executable" v "$fake" v0.1.0
reject "modified executable (--sha256)" "(modified executable" "$verify" --sha256 "$(cut -d' ' -f1 "$tmp/checksums.txt")" "$fake" v0.1.0
[ ! -e "$ran" ] || { echo "FAIL: a modified executable was run" >&2; exit 1; }
echo "ok: a modified executable is never run"
reject "asset absent from checksums" "0 entries for other" "$verify" --checksums "$tmp/checksums.txt" --name other "$fake" v0.1.0

echo "test-verify-release: ok"
