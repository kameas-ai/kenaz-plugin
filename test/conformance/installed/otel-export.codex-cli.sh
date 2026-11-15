#!/bin/sh
# Installed-tier journey (credential-free; not a live pass): otel-export on codex-cli.
exec "$(dirname "$0")/journey.sh" otel-export codex-cli
