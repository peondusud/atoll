# Development entry points (CLAUDE.md, "Stack and commands").

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN           := bin/atoll

.PHONY: all build test test-race conformance test-integration test-loopdev lint clean

all: build

build:
	$(GO) build -o $(BIN) ./cmd/atoll

# Unit and property tests, including conformance/vectors; no root.
test:
	$(GO) test ./...

# Mandatory before any commit touching jobs, ops, watch, integrity or catalog.
test-race:
	$(GO) test -race ./...

# Black-box: conformance/scenarios against the binary, through its CLI only.
conformance: build
	$(GO) run ./conformance/runner --bin $(BIN) --scenarios conformance/scenarios

# The next two depend on the machine, not only on the code, so they never reuse
# a cached result (-count=1).

# Disks simulated with directories; no root.
test-integration:
	$(GO) test -count=1 -tags integration ./test/...

# Needs root: ext4, NTFS, XFS, btrfs on loop devices; USB drops with dm-error / dm-flakey.
test-loopdev:
	$(GO) test -count=1 -tags loopdev ./test/...

lint:
	$(GOLANGCI_LINT) run ./...

clean:
	rm -rf bin
