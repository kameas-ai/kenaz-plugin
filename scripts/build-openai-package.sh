#!/usr/bin/env bash
# Builds the directory submission ZIP for the hosted Kameas Fleet connector
# from openai/ (see openai/SUBMISSION.md).
#
#   scripts/build-openai-package.sh [OUT_ZIP]
#
# OUT_ZIP defaults to dist/openai/kenaz-<version>.zip. The archive is
# deterministic: entries are sorted, timestamps and permissions are fixed,
# and only package files are included (SUBMISSION.md and other notes stay
# out). The plugin files sit at the archive root. TestOpenAIPackage checks
# the result.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
src="$root/openai"
version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$src/.codex-plugin/plugin.json")"
out="${1:-$root/dist/openai/kenaz-$version.zip}"
mkdir -p "$(dirname "$out")"

python3 -I - "$src" "$out" <<'PY'
import os
import sys
import zipfile

src, out = sys.argv[1], sys.argv[2]
# Package files only. Anything else under openai/ is documentation for us.
EXCLUDE = {"SUBMISSION.md", ".DS_Store"}

paths = []
for dirpath, dirnames, filenames in os.walk(src):
    dirnames.sort()
    for name in filenames:
        if name in EXCLUDE:
            continue
        full = os.path.join(dirpath, name)
        if os.path.islink(full) or not os.path.isfile(full):
            sys.exit(f"{full}: only regular files may go in the package")
        paths.append(os.path.relpath(full, src).replace(os.sep, "/"))
paths.sort()

tmp = out + ".tmp"
with zipfile.ZipFile(tmp, "w") as z:
    for rel in paths:
        info = zipfile.ZipInfo(rel, date_time=(1980, 1, 1, 0, 0, 0))
        info.compress_type = zipfile.ZIP_DEFLATED
        info.external_attr = 0o100644 << 16
        info.create_system = 3  # Unix, so the mode above is honoured
        with open(os.path.join(src, rel), "rb") as f:
            z.writestr(info, f.read(), compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
os.replace(tmp, out)
print(out)
PY
