#!/bin/sh
# Installed-tier (credential-free) conformance journey for one feature and
# host: test/conformance/README.md says what it proves and what it does not.
#
#   installed/journey.sh FEATURE HOST
#
# It builds this checkout's unofficial kenaz and runs it in a throwaway
# directory. It never signs in, never makes a model call and never contacts
# Kameas Fleet. KENAZ_CONFORMANCE_TAGS picks the build tags (default: none);
# the result goes to the file named by KENAZ_CONFORMANCE_STEPS, else stdout.
set -eu
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$root"
exec "${GO:-go}" run ./test/conformance/cmd/conformance journey -feature "$1" -host "$2" -root "$root"
