BINARY := octap
PKG    := github.com/wolffseb/ocpp-tap

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The harness charger: headless-csms's `cpms simulate charger`, built from a
# pinned commit. Run, not imported (see NOTICE and docs/PRD.md SP0).
HEADLESS_CSMS_REPO := https://github.com/wolffseb/headless-csms
HEADLESS_CSMS_SHA  := 603c2d112c736ff98e44844a57fb63dbda7096e3
CACHE              := $(CURDIR)/.cache
SIMULATOR          := $(CACHE)/bin/cpms-$(HEADLESS_CSMS_SHA)

# The proxy under test. Any binary taking -listen ADDR -upstream URL works;
# SP1 swaps in octap.
PROXY ?= $(CURDIR)/bin/placeholder-proxy

.PHONY: all build test race vet lint fmt check-fmt tidy clean ci e2e harness-deps placeholder

all: build

## build: compile octap into bin/
build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/octap

## test: run the unit tests
test:
	go test ./...

## race: run the unit tests under the race detector
race:
	go test -race ./...

vet:
	go vet ./...

## lint: golangci-lint v2 (CI pins v2.6.2)
lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

check-fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out e2e-artifacts

## ci: the automated gate's unit part, exactly as CI runs it
ci: check-fmt vet lint race

## harness-deps: build the pinned headless-csms simulator into .cache/
harness-deps: $(SIMULATOR)

$(SIMULATOR):
	rm -rf $(CACHE)/headless-csms
	git init -q $(CACHE)/headless-csms
	git -C $(CACHE)/headless-csms fetch -q --depth 1 $(HEADLESS_CSMS_REPO) $(HEADLESS_CSMS_SHA)
	git -C $(CACHE)/headless-csms -c advice.detachedHead=false checkout -q FETCH_HEAD
	cd $(CACHE)/headless-csms && go build -o $(SIMULATOR) ./cmd/cpms

placeholder:
	go build -o bin/placeholder-proxy ./harness/cmd/placeholder-proxy

## e2e: charger -> proxy -> CPMS through the SP0 harness, all scenarios
## (the -timeout is the PRD's 3-minute budget)
e2e: harness-deps placeholder
	E2E_SIMULATOR=$(SIMULATOR) E2E_PROXY=$(PROXY) \
		go test -tags e2e -count=1 -timeout 3m -v ./e2e/...
