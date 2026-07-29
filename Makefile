# Ambar build targets.
#
# GO and DOCKER are overridable so an environment where the toolchain is not on
# PATH can supply its own invocation without this file encoding a local quirk.
# For example, from inside a Flatpak editor sandbox:
#
#   make test GO="flatpak-spawn --host /usr/local/go/bin/go"
#   make docker DOCKER="flatpak-spawn --host podman"

GO     ?= go
GOFMT  ?= gofmt
DOCKER ?= docker

BIN     := dist/ambar
PKG     := ./cmd/ambar
IMAGE   ?= ghcr.io/datcal/ambar
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# -s -w strips the symbol table and DWARF: smaller binary, and a stack trace
# still carries function names.
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# CGO_ENABLED=0 is not optional. It is CLAUDE.md invariant 6 and the reason
# modernc.org/sqlite is used; see internal/db/fts5_test.go.
export CGO_ENABLED := 0

# Local run targets against ./testdata rather than the production paths.
TESTDATA_LIBRARY := $(CURDIR)/testdata/library
TESTDATA_DATA    := $(CURDIR)/testdata/data

.PHONY: all build test test-race vet fmt fmt-check tidy run scan user-add clean docker docker-run check

all: check build

build:
	@mkdir -p dist
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)
	@echo "built $(BIN) ($(VERSION), $(COMMIT))"

test:
	$(GO) test ./...

# The rate limiter and the session store are shared mutable state on the request
# path, so the race detector earns its runtime here.
#
# -race requires cgo and a C toolchain, so this one target overrides the global
# CGO_ENABLED=0. That does NOT weaken CLAUDE.md invariant 6: the invariant is
# about the binary that ships, and `make build` and the Dockerfile both stay at
# CGO_ENABLED=0. Skipped rather than failed where no C compiler exists.
test-race:
	@if ! command -v gcc >/dev/null 2>&1 && ! command -v clang >/dev/null 2>&1; then \
		echo "skipping: -race needs a C compiler, and none is on PATH"; exit 0; \
	fi; \
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GOFMT) -w .

fmt-check:
	@unformatted=$$($(GOFMT) -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

tidy:
	$(GO) mod tidy

# What CI should run.
check: fmt-check vet test

$(TESTDATA_LIBRARY) $(TESTDATA_DATA):
	@mkdir -p $@

# Run locally against ./testdata. AMBAR_COOKIE_SECURE stays on auto, which
# resolves to false for an http:// base URL — see config.Config.CookieSecure.
run: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	AMBAR_BIND=127.0.0.1:8080 \
	AMBAR_BASE_URL=http://localhost:8080 \
	AMBAR_LOG_LEVEL=debug \
	$(BIN) serve

# Index ./testdata. --dry-run first is the habit worth having on a real library.
scan: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) scan $(ARGS)

# Convenience wrapper for the first run, where the database has no users yet.
user-add: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	@test -n "$(USERNAME)" || { echo "usage: make user-add USERNAME=<name>"; exit 2; }
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) user add $(USERNAME)

docker:
	$(DOCKER) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
	@echo
	@$(DOCKER) images $(IMAGE)

# Smoke-test the image against the same testdata directories. --user matches the
# host owner so the bind mounts stay writable, which is the §17 requirement in
# miniature: get it wrong and the container refuses to start.
#
# ROOTLESS_FLAGS is needed for rootless podman only. There, --user alone maps to
# a subuid that does not own the bind mount, so --userns=keep-id is required as
# well. Real Docker passes host uids straight through and needs neither, which is
# why docker-compose.yml only sets `user:`.
#
#   make docker-run DOCKER=podman ROOTLESS_FLAGS=--userns=keep-id
ROOTLESS_FLAGS ?=

docker-run: | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	$(DOCKER) run --rm -it \
		$(ROOTLESS_FLAGS) \
		--user "$$(id -u):$$(id -g)" \
		-p 8973:8080 \
		-v $(TESTDATA_LIBRARY):/library \
		-v $(TESTDATA_DATA):/data \
		-e AMBAR_BASE_URL=http://localhost:8973 \
		$(IMAGE):latest serve

clean:
	rm -rf dist
	$(GO) clean -testcache
