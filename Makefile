BINARY := octap
PKG    := github.com/wolffseb/ocpp-tap

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race vet lint fmt check-fmt tidy clean ci e2e

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

## e2e: charger -> proxy -> CPMS through the SP0 harness
e2e:
	@echo "e2e: SP0 harness not implemented yet" >&2; exit 1
