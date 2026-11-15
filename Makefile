GO      ?= go
PYTHON  ?= python3
VERSION ?= 0.0.0-dev
TMPDIR  ?= /tmp
LDFLAGS := -X github.com/kameas-ai/kenaz-plugin/app.Version=$(VERSION)
# TAGS=kenaz_opencode turns on OpenCode support (off in released builds).
TAGS    ?=
# The official build's tags, minus `fleet`: that one links the proprietary
# Fleet client, which this repository cannot compile. Everything else the
# official build turns on compiles and is tested here (test-official-tags).
OFFICIAL_PUBLIC_TAGS ?= kenaz_memory_capture,kenaz_memory_project_scope,kenaz_context_library,kenaz_claude_desktop_chat
# The packages the kenaz_opencode tag changes (test-opencode-tag).
OPENCODE_TAG_PKGS ?= ./app/ ./internal/features/ ./internal/fleetschema/ ./internal/project/ ./internal/setup/ ./internal/skills/ ./internal/supported/

.PHONY: build test test-opencode test-official-tags print-official-public-tags test-opencode-tag conformance-installed vet fmt-check check check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks conformance-validate conformance clean

build:
	$(GO) build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" -o bin/kenaz ./cmd/kenaz

test:
	$(GO) test -race -tags "$(TAGS)" ./...

# The OpenCode adapter's tests need Node 18+ and no network. They are not
# part of `check`, which needs only Go and Python.
NODE    ?= node
test-opencode:
	cd opencode && $(NODE) --test

# The whole suite with every official build tag this repository can compile
# together (see OFFICIAL_PUBLIC_TAGS). Not part of `check`.
test-official-tags:
	$(GO) vet -tags "$(OFFICIAL_PUBLIC_TAGS)" ./...
	$(GO) test -race -tags "$(OFFICIAL_PUBLIC_TAGS)" ./...

print-official-public-tags:
	@echo $(OFFICIAL_PUBLIC_TAGS)

# The Go side of OpenCode support (build tag kenaz_opencode, in no released
# build): vet everything, test the packages the tag changes. Not part of
# `check`.
test-opencode-tag:
	$(GO) vet -tags kenaz_opencode ./...
	$(GO) test -race -tags kenaz_opencode $(OPENCODE_TAG_PKGS)

vet:
	$(GO) vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check-no-policy:
	$(PYTHON) scripts/checks.py no-policy .

check-names:
	$(PYTHON) scripts/checks.py names .

check-legacy-name:
	$(PYTHON) scripts/checks.py legacy-name .

check-privacy-logs:
	$(PYTHON) scripts/check-privacy-logs.py .

check-no-auto-accept:
	$(GO) test ./internal/consent -run TestNoAutoAccept -count=1

check-notices:
	scripts/third-party-notices.sh $(TMPDIR)/kenaz-tpn && cmp -s $(TMPDIR)/kenaz-tpn THIRD_PARTY_NOTICES || { echo "THIRD_PARTY_NOTICES is stale: run scripts/third-party-notices.sh"; exit 1; }

test-checks:
	bash scripts/test-checks.sh

# Baseline conformance: validate the compatibility lock and the 24-cell (Claude Code and Codex)
# matrix (fast, offline; part of check), or run every cell for this
# machine's OS and build the release verdict (not part of check).
conformance-validate:
	$(GO) run ./test/conformance/cmd/conformance validate

conformance:
	$(GO) run ./test/conformance/cmd/conformance run
	-$(GO) run ./test/conformance/cmd/conformance verdict

# The credential-free installed tier (test/conformance/README.md): builds
# this checkout's unofficial kenaz and runs it in throwaway directories for
# this machine's OS. No sign-in, no model call, no Kameas Fleet. Its results
# are recorded apart from the live tier and never satisfy the release gate.
# TAGS picks the build tags, e.g. TAGS=$(OFFICIAL_PUBLIC_TAGS). It fails
# unless all 8 cells of this machine's OS pass: on a machine that is not a
# baseline OS target every cell is blocked, which must not look like success.
conformance-installed:
	KENAZ_CONFORMANCE_TAGS="$(TAGS)" $(GO) run ./test/conformance/cmd/conformance run -tier installed -min-pass 8
	$(GO) run ./test/conformance/cmd/conformance verdict -tier installed
	$(GO) run ./test/conformance/cmd/conformance report

check: fmt-check vet test check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks conformance-validate

clean:
	rm -rf bin
