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

.PHONY: all test e2e not_e2e binaries ts vsh vshd thundersnapd bupdate tsm fidx slab \
        list build build-deb build-rpm build-tgz clean

all: build

# Run all tests (requires CGO_ENABLED=0 for cmd/ts tests).
# If the tests pass, also verify all Go files are gofmt-formatted; fail if not.
test:
	CGO_ENABLED=0 go test ./...
	@echo "checking gofmt..."
	@unformatted=$$(gofmt -l $$(find . -name '*.go' -not -path './.tmp-e2e/*' -not -path './vendor/*' 2>/dev/null)); \
	if [ -n "$$unformatted" ]; then \
		echo "ERROR: the following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		echo "run 'gofmt -w .' to fix"; \
		exit 1; \
	fi
	@echo "gofmt OK"

# Run e2e tests (requires root and btrfs)
# These are true end-to-end tests that start a real thundersnapd and SSH into it.
# Compiles the test binary and dependencies as the current user, then runs with sudo.
# TMPDIR must be on btrfs (not /tmp which is typically tmpfs).
# -test.timeout is intentionally aggressive (well under Go's 10m default): these
# tests are all fast, so a hang almost always means a real bug, not a slow test.
E2E_TMPDIR ?= $(CURDIR)/.tmp-e2e
E2E_TEST_TIMEOUT ?= 2m
NOT_E2E_TEST_TIMEOUT ?= 2m
e2e: ts vshd thundersnapd
	@mkdir -p $(E2E_TMPDIR)
	CGO_ENABLED=0 go test -tags e2e -c -o $(BIN)/e2e.test ./e2e
	sudo -E env \
		TMPDIR="$(E2E_TMPDIR)" \
		TS_BINARY="$(CURDIR)/$(BIN)/ts" \
		VSHD_BINARY="$(CURDIR)/$(BIN)/vshd" \
		THUNDERSNAPD_BINARY="$(CURDIR)/$(BIN)/thundersnapd" \
		$(BIN)/e2e.test -test.v -test.failfast -test.timeout=$(E2E_TEST_TIMEOUT) $(E2E_ARGS)

# Run legacy "e2e" tests (not actually e2e - see not-e2e-enough.md)
# These tests exercise individual components but don't go through the SSH front door.
not_e2e: ts vshd thundersnapd
	@mkdir -p $(E2E_TMPDIR)
	CGO_ENABLED=0 go test -tags e2e -c -o $(BIN)/not_e2e.test ./not_e2e
	sudo -E env \
		TMPDIR="$(E2E_TMPDIR)" \
		TS_BINARY="$(CURDIR)/$(BIN)/ts" \
		VSHD_BINARY="$(CURDIR)/$(BIN)/vshd" \
		THUNDERSNAPD_BINARY="$(CURDIR)/$(BIN)/thundersnapd" \
		$(BIN)/not_e2e.test -test.v -test.failfast -test.timeout=$(NOT_E2E_TEST_TIMEOUT)

# Build all binaries for local development
binaries: ts vsh vshd thundersnapd bupdate tsm fidx slab

# Binaries that need CGO_ENABLED=0 (run inside containers/VMs)
ts:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -o $(BIN)/$@ ./cmd/$@

vshd:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -o $(BIN)/$@ ./cmd/$@

# Binaries that can use default CGO setting
vsh:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

thundersnapd:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

bupdate:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

tsm:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

fidx:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

slab:
	@mkdir -p $(BIN)
	go build -o $(BIN)/$@ ./cmd/$@

# List all available build targets
list:
	$(DIST_CMD) list

# Build all packages (deb, rpm, tgz for all architectures)
build:
	$(DIST_CMD) build --out "$(OUT)" all

# Build only .deb packages
build-deb:
	$(DIST_CMD) build --out "$(OUT)" "deb"

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
