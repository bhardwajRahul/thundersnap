# Thundersnap Makefile
#
# Development targets:
#   make test       - run all tests
#   make e2e        - run e2e tests (requires sudo and btrfs)
#   make binaries   - build all binaries for local development
#   make ts         - build just the ts binary
#
# Distribution targets:
#   make build      - build distribution packages (deb, rpm, tgz)
#   make build-deb  - build only .deb packages
#   make list       - list all available build targets
#
# Note: cmd/ts requires CGO_ENABLED=0 because it runs inside containers/VMs
# where dynamically linked binaries may not work. The Makefile handles this.

DIST_CMD = go run ./cmd/dist

# Default output directory for packages
OUT ?= dist

# Output directory for local binaries
BIN ?= ./bin

.PHONY: all test e2e e2e-tb not_e2e binaries ts vshd thundersnapd thunderboot infiniblockd \
        thunderboot-vm-artifacts thunderboot-appliance-arm64 verify-thunderboot-appliance-arm64 \
        list build build-deb build-rpm build-tgz clean

all: build

# Run all unit tests (requires CGO_ENABLED=0 for cmd/ts tests).
#
# Two timeouts, layered:
#   - go test -timeout 30s: PER-PACKAGE soft timeout. go test compiles each
#     package into its own test binary and runs them in turn; -timeout caps
#     each binary's run, so a 5s test that takes 29s counts as a pass but one
#     that hits 30s panics (a should-be-fast test running long is caught).
#     The build (go test -c, cache misses, module downloads) is NOT counted
#     against this -- only the test run.
#   - external `timeout -s KILL 60s`: GLOBAL hard safety net. go test's
#     -timeout fires a panic on the test goroutine, which a goroutine blocked
#     in a syscall (e.g. a test hung on a socket read) won't receive until the
#     syscall returns -- so a truly hung test may not die from -timeout alone.
#     The external SIGKILL guarantees the whole run ends. 60s is generous:
#     unit tests are sub-second (for contrast the e2e suite boots real KVM VMs
#     in ~0.1s and whole tests finish in seconds), so hitting either timeout
#     almost always means a real hang/bug, not a slow test.
#
# If the tests pass, also verify all Go files are gofmt-formatted; fail if not.
test:
	CGO_ENABLED=0 timeout -s KILL 60s go test -timeout 30s ./...
	@echo "checking gofmt..."
	@unformatted=$$(gofmt -l $$(find . -name '*.go' -not -path './.tmp-e2e/*' -not -path './vendor/*' 2>/dev/null)); \
	if [ -n "$$unformatted" ]; then \
		echo "ERROR: the following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		echo "run 'gofmt -w .' to fix"; \
		exit 1; \
	fi
	@echo "gofmt OK"

# Run e2e tests (requires root and btrfs).
# These are true end-to-end tests that start a real thundersnapd and SSH into it.
# Compiles the test binary and dependencies as the current user (untimed), then
# runs with sudo. TMPDIR must be on btrfs (not /tmp which is typically tmpfs).
#
# Timeouts are extremely generous relative to reality: the tests boot real KVM
# VMs in ~0.1s and each test finishes in a few seconds, so the 2m go-test
# -test.timeout and the 120s Makefile-level hard cap both leave enormous
# headroom — hitting either almost always means a real hang/bug, not a slow
# test. E2E_ARGS can be used to pass extra args (e.g., E2E_ARGS="-run TestFoo").
#
# test-cleanup.sh reclaims leftover btrfs subvols and sets .tmp-e2e to 0700
# root before the run (clean slate) and after a SUCCESSFUL run (reclaim
# orphans). On failure the cleanup is skipped so orphans remain for debugging.
E2E_TMPDIR ?= $(CURDIR)/.tmp-e2e
E2E_TEST_TIMEOUT ?= 2m
E2E_ARGS ?=
NOT_E2E_TEST_TIMEOUT ?= 2m
e2e: ts vshd thundersnapd
	@mkdir -p $(E2E_TMPDIR)
	@./test-cleanup.sh $(E2E_TMPDIR)
	CGO_ENABLED=0 go test -tags e2e -c -o $(BIN)/e2e.test ./e2e
	sudo -E timeout 120s env \
		TMPDIR="$(E2E_TMPDIR)" \
		TS_BINARY="$(CURDIR)/$(BIN)/ts" \
		VSHD_BINARY="$(CURDIR)/$(BIN)/vshd" \
		THUNDERSNAPD_BINARY="$(CURDIR)/$(BIN)/thundersnapd" \
		$(BIN)/e2e.test -test.v -test.failfast -test.timeout=$(E2E_TEST_TIMEOUT) $(E2E_ARGS)
	@./test-cleanup.sh $(E2E_TMPDIR)

# Run thunderboot storage-layout tests.
e2e-tb: thunderboot infiniblockd thunderboot-vm-artifacts
	./scripts/build-thunderboot-initramfs.sh
	CGO_ENABLED=0 go test -tags e2e -c -o $(BIN)/e2e-tb.test ./e2e-tb
	sudo -E timeout 300s $(BIN)/e2e-tb.test -test.v -test.failfast -test.timeout=5m $(E2E_ARGS)

# Run legacy "e2e" tests (not actually e2e - see not-e2e-enough.md).
# These tests exercise individual components but don't go through the SSH front
# door. Same timeout philosophy as `e2e`: the 2m go-test -test.timeout and 240s
# Makefile-level hard cap are both extremely generous (the tests, including the
# ones that boot KVM VMs, finish in seconds); a timeout means a real hang/bug.
# test-cleanup.sh runs before (clean slate) and after a successful run; on
# failure it is skipped so orphans remain for debugging.
not_e2e: ts vshd thundersnapd
	@mkdir -p $(E2E_TMPDIR)
	@./test-cleanup.sh $(E2E_TMPDIR)
	CGO_ENABLED=0 go test -tags e2e -c -o $(BIN)/not_e2e.test ./not_e2e
	sudo -E timeout 240s env \
		TMPDIR="$(E2E_TMPDIR)" \
		TS_BINARY="$(CURDIR)/$(BIN)/ts" \
		VSHD_BINARY="$(CURDIR)/$(BIN)/vshd" \
		THUNDERSNAPD_BINARY="$(CURDIR)/$(BIN)/thundersnapd" \
		$(BIN)/not_e2e.test -test.v -test.failfast -test.timeout=$(NOT_E2E_TEST_TIMEOUT) $(E2E_ARGS)
	@./test-cleanup.sh $(E2E_TMPDIR)

# Build all binaries for local development
binaries: ts vshd thundersnapd thunderboot

# Binaries that need CGO_ENABLED=0 (run inside containers/VMs)
ts:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -o $(BIN)/$@ ./cmd/$@

vshd:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -o $(BIN)/$@ ./cmd/$@

# Binaries that can use default CGO setting
# thundersnapd: CGO_ENABLED=0 so the nested test (nested_test.go) can run it
# inside a minimal container rootfs that lacks shared libraries.
thundersnapd:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -o $(BIN)/$@ ./cmd/$@

thunderboot:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

infiniblockd:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

# Generate the untracked x86-64 Cloud Hypervisor and kernel artifacts used by
# Linux/KVM tests. See README.thunderboot-builder.md.
thunderboot-vm-artifacts:
	./scripts/fetch-thunderboot-vm-artifacts.sh

# Build and package the ARM64 Aperture kernel/initramfs in pinned Lima.
thunderboot-appliance-arm64:
	./scripts/thunderboot-builder.sh build

verify-thunderboot-appliance-arm64:
	./scripts/thunderboot-builder.sh verify

# List all available build targets
list:
	$(DIST_CMD) list

# Build all packages (deb, rpm, tgz for all architectures)
build:
	$(DIST_CMD) build --out "$(OUT)" all

# Build only .deb packages
build-deb:
	$(DIST_CMD) build --out "$(OUT)" "deb"

# Build only the thundersnap-dev .deb package (for blue/green deployment)
build-deb-dev:
	$(DIST_CMD) build --out "$(OUT)" "deb-dev"

# Build only .rpm packages
build-rpm:
	$(DIST_CMD) build --out "$(OUT)" "rpm"

# Build only .tgz tarballs
build-tgz:
	$(DIST_CMD) build --out "$(OUT)" "tgz"

# Build for a specific architecture (e.g., make build-amd64, make build-arm64)
build-amd64:
	$(DIST_CMD) build --out "$(OUT)" "linux/amd64"

build-arm64:
	$(DIST_CMD) build --out "$(OUT)" "linux/arm64"

clean:
	rm -rf "$(OUT)" "$(BIN)"
