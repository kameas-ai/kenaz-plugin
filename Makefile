GO      ?= go
PYTHON  ?= python3
VERSION ?= 0.0.0-dev
TMPDIR  ?= /tmp
LDFLAGS := -X github.com/kameas-ai/kenaz-plugin/app.Version=$(VERSION)

.PHONY: build test vet fmt-check check check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/kenaz ./cmd/kenaz

test:
	$(GO) test -race ./...

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

check: fmt-check vet test check-no-policy check-names check-legacy-name check-privacy-logs check-no-auto-accept check-notices test-checks

clean:
	rm -rf bin
