# Development entry points (CLAUDE.md, "Stack and commands").

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN           := bin/atoll

.PHONY: all build test test-race conformance model-check model-flaws test-integration test-loopdev qemu-build test-qemu test-mediaservers lint release-snapshot clean

all: build

build:
	$(GO) build -o $(BIN) ./cmd/atoll

# Unit and property tests, including conformance/vectors; no root.
test:
	$(GO) test ./...

# Mandatory before any commit touching jobs, ops, watch, integrity or catalog.
test-race:
	$(GO) test -race -timeout 45m ./...

# Black-box: conformance/scenarios against a conformance build, through its CLI only.
conformance:
	$(GO) build -tags conformance -o $(BIN)-conformance ./cmd/atoll
	$(GO) run ./conformance/runner --bin $(BIN)-conformance --scenarios conformance/scenarios

# The TLA+ model of conformance/models/, each instance against what it expects, by TLC in a container with 2 workers:
# only Docker, and Java never outside the container (test/tla/run.sh).
model-check:
	bash test/tla/run.sh

# The black-box scenarios of the flaws the model found in specs.md and still open (conformance/models/flaws/): they
# fail until the maintainer decides a remedy, and then move to conformance/scenarios/. With none open, the folder is gone.
model-flaws:
ifeq ($(wildcard conformance/models/flaws/*.json),)
	@echo "model-flaws: no flaw open (conformance/models/README.md)"
else
	$(GO) build -tags conformance -o $(BIN)-conformance ./cmd/atoll
	$(GO) run ./conformance/runner --bin $(BIN)-conformance --scenarios conformance/models/flaws
endif

# The next two depend on the machine, not only on the code, so they never reuse
# a cached result (-count=1).

# Disks simulated with directories; no root.
test-integration:
	$(GO) test -count=1 -tags integration ./test/...

# Needs root: ext4, NTFS, XFS, btrfs on loop devices; USB drops with dm-error / dm-flakey.
test-loopdev:
	$(GO) test -count=1 -tags loopdev ./test/...

# What test-qemu boots: the loopdev tests, static, with what they read above their folder; and atoll itself, which
# deploy/'s units start in the guest's run under systemd.
qemu-build:
	rm -rf bin/qemu
	mkdir -p bin/qemu/repo
	CGO_ENABLED=0 $(GO) test -c -tags loopdev,memory.counters -o bin/qemu/loopdev.test ./test/loopdev
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/qemu/atoll ./cmd/atoll
	cp go.mod bin/qemu/repo/
	cp -r conformance deploy bin/qemu/repo/

# test-loopdev as root in qemu, on Debian's kernel and a mainline one, with neither root nor ntfs3 on
# the host: only Docker (test/qemu/README.md). QEMU_CACHE, a folder of the host, keeps the packages and
# the kernel it downloads from one run to the next, as the CI's qemu job does.
test-qemu: qemu-build
	docker run --rm $(if $(wildcard /dev/kvm),--device /dev/kvm) -e KERNELS -e MAINLINE -e RUN -e SYSTEMD_RUN -e COUNT -e VERBOSE \
		$(if $(QEMU_CACHE),-v "$(abspath $(QEMU_CACHE)):/var/cache/atoll-qemu" -e CACHE=/var/cache/atoll-qemu) \
		-v "$(CURDIR):/src:ro" -v "$(CURDIR)/bin/qemu:/qemu:ro" debian:13 bash /src/test/qemu/run.sh

# The notifications on real Plex, Jellyfin and Emby, each in a container, on the last commit: only git
# and Docker (test/mediaservers/run.sh).
test-mediaservers:
	bash test/mediaservers/run.sh

# The coverage of the unit tests and of the conformance scenarios (test/ci/cover.sh), into bin/cover/: one profile of
# the whole module by the unit tests, each package's tests counting toward every package they reach; one by the
# scenarios, run against a conformance build made with -cover; then the share of statements each runs, and either, by
# package and in all, the time each took, and the profiles as HTML; then the floor of test/ci/gates.txt, which the total
# of the unit tests and the merged one must each meet. The CI's cover job runs it.
.PHONY: cover
cover:
	GO="$(GO)" bash test/ci/cover.sh

# Mutation testing with gremlins (test/ci/mutation.sh) of one package, PKG=./internal/glob, or of the whole pure core
# without PKG, as .github/workflows/mutation.yml runs it each night. The log and gremlins' JSON go to bin/mutation/;
# WORKERS mutants at once, the CPUs by default. A package of test/ci/gates.txt under its gate fails it, the mutants
# test/ci/mutation-equivalents.txt names left out (test/ci/mutation-gate.sh).
.PHONY: mutation
mutation:
	bash test/ci/mutation.sh $(PKG)

# The fuzz targets (test/ci/fuzz.sh), each for FUZZTIME, 1m by default, one after the other, or the one PKG and TARGET
# name, as .github/workflows/fuzz.yml runs each for longer every night. A failing input goes to bin/fuzz/ too.
.PHONY: fuzz
fuzz:
	bash test/ci/fuzz.sh $(if $(TARGET),$(PKG) $(TARGET))

# The conformance build holds code the delivered binary does not, so it gets a run of its own.
lint:
	$(GOLANGCI_LINT) run ./...
	$(GOLANGCI_LINT) run --build-tags conformance ./cmd/... ./internal/disks/...

# A release as .github/workflows/release.yml makes one (.goreleaser.yaml), into dist/, from this checkout and
# what it holds uncommitted: no tag, no upload, only Docker. From a clone: a git worktree's .git names a
# folder the container does not have. What the container writes goes back to whoever runs make. The image's
# GOTOOLCHAIN is local: auto lets its go fetch go.mod's version when the image holds an older one, so the build
# takes the Go a release takes.
GORELEASER_IMAGE := goreleaser/goreleaser:v2.20.3
release-snapshot:
	docker run --rm -v "$(CURDIR):/src" -w /src -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory \
		-e GIT_CONFIG_VALUE_0=/src -e GOTOOLCHAIN=auto --entrypoint sh $(GORELEASER_IMAGE) -c \
		'goreleaser release --snapshot --clean; s=$$?; chown $(shell id -u):$(shell id -g) bin 2>/dev/null; \
		chown -R $(shell id -u):$(shell id -g) bin/deb dist 2>/dev/null; exit $$s'

clean:
	rm -rf bin dist
