DIST_CMD = go run ./cmd/dist

# Default output directory for packages
OUT ?= dist

.PHONY: all list build build-deb build-rpm build-tgz clean

all: build

# List all available build targets
list:
	$(DIST_CMD) list

# Build all packages (deb, rpm, tgz for all architectures)
build:
	$(DIST_CMD) build --out $(OUT) all

# Build only .deb packages
build-deb:
	$(DIST_CMD) build --out $(OUT) "deb"

# Build only .rpm packages
build-rpm:
	$(DIST_CMD) build --out $(OUT) "rpm"

# Build only .tgz tarballs
build-tgz:
	$(DIST_CMD) build --out $(OUT) "tgz"

# Build for a specific architecture (e.g., make build-amd64, make build-arm64)
build-amd64:
	$(DIST_CMD) build --out $(OUT) "linux/amd64"

build-arm64:
	$(DIST_CMD) build --out $(OUT) "linux/arm64"

clean:
	rm -rf $(OUT)
