GO      ?= go
PYTHON  ?= python3
VERSION ?= 0.0.0-dev
TMPDIR  ?= /tmp
LDFLAGS := -X github.com/kameas-ai/kenaz-plugin/app.Version=$(VERSION)
# TAGS=kenaz_opencode turns on OpenCode support (off in released builds).
TAGS    ?=

.PHONY: build test test-opencode vet fmt-check check check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks conformance-validate conformance clean

build:
	$(GO) build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" -o bin/kenaz ./cmd/kenaz

test:
	$(GO) test -race -tags "$(TAGS)" ./...

# The OpenCode adapter's tests need Node 18+ and no network. They are not
# part of `check`, which needs only Go and Python.
NODE    ?= node
test-opencode:
	cd opencode && $(NODE) --test

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

check: fmt-check vet test check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks conformance-validate

clean:
	rm -rf bin
