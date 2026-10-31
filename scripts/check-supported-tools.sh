#!/usr/bin/env bash
# check-supported-tools.sh [<supported-tools.json>]
#
# Fails when the docs Supported Tools list and this source tree drift apart.
# With no argument it fetches the live list the docs site publishes
# (https://docs.kameas.ai/kenaz-plugin/supported-tools.json, a byte-for-byte
# copy of kenaz-docs docs/kenaz-plugin/supported-tools.json, the file the
# docs page renders; $SUPPORTED_TOOLS_URL overrides it) and checks it against the hosts this build supports
# (internal/supported.Hosts), using the same test as `make test` runs on the
# vendored copy. Run it from the root of this repository with the build tags
# of the release ($TAGS), as the official release workflow does. Here the
# list must match the build exactly: rows for feature-gated hosts
# (internal/supported.GatedHosts: the Claude desktop app's chat, tag
# kenaz_claude_desktop_chat, and Cowork, tag kenaz_cowork) must be listed
# with the feature's tag and must not be without it. (The
# vendored-copy check in `make test` is lenient about gated rows.)
#
# The vendored copy (internal/supported/testdata/supported-tools.json) must
# also equal the live list, so a docs change cannot land without this repo
# noticing.
set -euo pipefail
GO="${GO:-go}"
URL="${SUPPORTED_TOOLS_URL:-https://docs.kameas.ai/kenaz-plugin/supported-tools.json}"
VENDORED="internal/supported/testdata/supported-tools.json"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if [ $# -ge 1 ]; then
  cp "$1" "$tmp/live.json"
else
  if ! curl -fsSL --retry 3 "$URL" -o "$tmp/live.json"; then
    echo "::error::could not fetch the Supported Tools list from $URL (is kenaz-docs#24 deployed?)" >&2
    exit 1
  fi
fi

canon() { python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), sort_keys=True))' "$1"; }
if [ "$(canon "$tmp/live.json")" != "$(canon "$VENDORED")" ]; then
  echo "::error::the docs Supported Tools list differs from $VENDORED; update the vendored copy (or the docs) so they match" >&2
  diff <(python3 -m json.tool --sort-keys "$VENDORED") <(python3 -m json.tool --sort-keys "$tmp/live.json") >&2 || true
  exit 1
fi
KENAZ_SUPPORTED_TOOLS_JSON="$tmp/live.json" "$GO" test -count=1 -tags "${TAGS:-}" -run '^TestSupportedToolsMatchHosts$' ./internal/supported/
echo "supported tools: docs list matches this build"
