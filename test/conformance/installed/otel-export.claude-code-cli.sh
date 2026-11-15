#!/bin/sh
# Installed-tier journey (credential-free; not a live pass): otel-export on claude-code-cli.
exec "$(dirname "$0")/journey.sh" otel-export claude-code-cli
