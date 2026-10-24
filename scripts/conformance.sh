#!/usr/bin/env bash
# Baseline conformance harness (test/conformance).
#
#   scripts/conformance.sh validate
#   scripts/conformance.sh run [-out DIR] [-cell 'otel-export/*' ...]
#   scripts/conformance.sh verdict [-evidence DIR] [-out FILE]
#   scripts/conformance.sh check-verdict -verdict FILE -artifact OS=PATH ...
#
# `run` executes every cell for this machine's OS: the component journey
# (recorded fixtures against in-process fake Fleet providers) and, only
# when the lock is fully verified, the host is installed, a development
# Fleet identity is prepared (KENAZ_CONFORMANCE_DEV) and a recorded
# installed journey exists, that journey. Anything missing is recorded as
# blocked with its gate, never as a pass. Cells for other OSes are
# blocked until run on that OS's runner.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
exec go run ./test/conformance/cmd/conformance "$@" -root "$root"
