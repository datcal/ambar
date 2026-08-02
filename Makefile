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

# Local dev port for `make run`. Overridable, because the container default of
# 8080 is regularly taken on a dev box (an IDE, another service). 8973 is Ambar's
# own host-facing port elsewhere in this file, so it is the memorable default;
# override with `make run PORT=9001` if that one is busy too.
PORT   ?= 8973

BIN     := dist/ambar
PKG     := ./cmd/ambar
IMAGE   ?= ghcr.io/datcal/ambar
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# -s -w strips the symbol table and DWARF: smaller binary, and a stack trace
# still carries function names.
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# CGO_ENABLED=0 is not optional. It is ARCHITECTURE.md rule 6 and the reason
# modernc.org/sqlite is used; see internal/db/fts5_test.go.
export CGO_ENABLED := 0

# Local run targets against ./testdata rather than the production paths.
TESTDATA_LIBRARY := $(CURDIR)/testdata/library
TESTDATA_DATA    := $(CURDIR)/testdata/data

.PHONY: all build test test-race vet fmt fmt-check tidy run scan derive dupes trash user-add clean docker docker-run deploy deploy-config check godot-test plugin-zip release-artifacts

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
# CGO_ENABLED=0. That does NOT weaken ARCHITECTURE.md rule 6: the invariant is
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

# The Godot plugin, packaged the way a Godot user expects it: unzip at the project root
# and you have res://addons/ambar/plugin.cfg. Nothing else from this repository goes in —
# the server is not the plugin's business.
#
# .uid files are included deliberately: Godot 4.4+ keys script references to them, and
# shipping stable ones means a project that upgrades the addon does not re-resolve every
# reference.
# Built with python3 rather than `zip`, which is not installed everywhere (it is absent
# from this repository's own CI image and from a plain Alpine).
PLUGIN_ZIP := dist/ambar-godot-plugin-$(VERSION).zip

plugin-zip:
	@mkdir -p dist
	@rm -f $(PLUGIN_ZIP)
	@python3 -c "import pathlib, zipfile; \
	root = pathlib.Path('addons/ambar'); \
	files = sorted(p for p in root.rglob('*') if p.is_file() and p.name != '.DS_Store'); \
	z = zipfile.ZipFile('$(PLUGIN_ZIP)', 'w', zipfile.ZIP_DEFLATED); \
	[z.write(p, p.as_posix()) for p in files]; \
	z.close(); \
	print('built $(PLUGIN_ZIP) — %d files' % len(files))"

# The Godot plugin's own suite. Not part of `check`: it needs a Godot binary, and two
# of the three passes need a running server to talk to. See godot-test/README.md.
#
#   make godot-test GODOT="/path/to/godot"                 parse check + API + import
#   make godot-test GODOT=… ARGS="ui"                      also render a screenshot
#
# The parse check alone catches the failure mode that made the M16 plugin appear to do
# nothing at all: a GDScript parse error leaves the addon enabled and inert, and says so
# only in the Output panel.
godot-test:
	@test -n "$(GODOT)" || { echo "usage: make godot-test GODOT=/path/to/godot"; exit 2; }
	@echo "== parse check"
	@"$(GODOT)" --headless --editor --quit --path $(CURDIR)/godot-test 2>&1 | grep -E "SCRIPT ERROR|Parse Error|Compile Error" && exit 1 || echo "   no script errors"
	@echo "== opening the tab"
	"$(GODOT)" --headless --script res://test_open.gd --path $(CURDIR)/godot-test
	@echo "== api"
	"$(GODOT)" --headless --script res://test_api.gd --path $(CURDIR)/godot-test
	@echo "== import"
	"$(GODOT)" --headless --script res://test_import.gd --path $(CURDIR)/godot-test
	@echo "== project screen"
	"$(GODOT)" --headless --script res://test_project.gd --path $(CURDIR)/godot-test
ifneq (,$(findstring ui,$(ARGS)))
	@echo "== models (needs a display: it renders)"
	"$(GODOT)" --script res://test_model.gd --path $(CURDIR)/godot-test
	@echo "== ui (needs a display)"
	"$(GODOT)" --script res://test_ui.gd --path $(CURDIR)/godot-test -- "" $(CURDIR)/godot-test/ui.png
endif

# Everything a release ships. Built here rather than inline in the workflow, so a release
# can be reproduced — and debugged — on a laptop with the same command CI runs.
#
# VERSION comes from `git describe`, so a tagged checkout produces the tag; pass it
# explicitly (VERSION=v1.0.0) to build what a tag would build without one.
RELEASE_PLATFORMS := linux/amd64 linux/arm64

release-artifacts: plugin-zip
	@rm -rf dist/stage && mkdir -p dist/stage
	@cp README.md LICENSE CHANGELOG.md dist/stage/
	@for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%%/*}; arch=$${platform##*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/stage/ambar $(PKG) || exit 1; \
		tar -C dist/stage -czf dist/ambar_$(VERSION)_$${os}_$${arch}.tar.gz \
			ambar README.md LICENSE CHANGELOG.md || exit 1; \
		rm dist/stage/ambar; \
	done
	@rm -rf dist/stage
	@cd dist && sha256sum ambar_$(VERSION)_*.tar.gz ambar-godot-plugin-$(VERSION).zip > checksums-$(VERSION).txt
	@echo
	@ls -lh dist/ambar_$(VERSION)_*.tar.gz dist/ambar-godot-plugin-$(VERSION).zip dist/checksums-$(VERSION).txt

$(TESTDATA_LIBRARY) $(TESTDATA_DATA):
	@mkdir -p $@

# Run locally against ./testdata. AMBAR_COOKIE_SECURE stays on auto, which
# resolves to false for an http:// base URL — see config.Config.CookieSecure.
#
# AMBAR_LOCAL_LIBRARY_PATH is the library as the *browser's* machine sees it, which is
# what the asset page's "Open in Aseprite / Blender / Godot" buttons and its copy-path
# control need (M15). Here the server and the browser are the same machine, so it is
# simply the absolute library root. It is not derived automatically because on the §17
# deployment the server's path (/library, inside the container) means nothing on the
# machine you are sitting at — there it has to be the SMB mount or the UNC path.
run: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	AMBAR_BIND=127.0.0.1:$(PORT) \
	AMBAR_BASE_URL=http://localhost:$(PORT) \
	AMBAR_LOG_LEVEL=debug \
	AMBAR_LOCAL_LIBRARY_PATH=$(abspath $(TESTDATA_LIBRARY)) \
	$(BIN) serve

# Index ./testdata. --dry-run first is the habit worth having on a real library.
scan: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) scan $(ARGS)

# Generate thumbnails and previews for anything missing them, then exit. The same work
# happens automatically while `make run` is going.
derive: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) derive $(ARGS)

# Report duplicate content against ./testdata (§9.1). Reporting only: nothing here
# can remove a file, and the removal path needs a selection and a confirmed preview.
dupes: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) dupes $(ARGS)

# Inspect the trash. `make trash ARGS="purge --older-than 720h"` to purge, which is
# the only irreversible operation in the whole application and never automatic.
trash: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) trash $(if $(ARGS),$(ARGS),list)

# Convenience wrapper for the first run, where the database has no users yet.
user-add: build | $(TESTDATA_LIBRARY) $(TESTDATA_DATA)
	@test -n "$(USERNAME)" || { echo "usage: make user-add USERNAME=<name>"; exit 2; }
	AMBAR_LIBRARY_ROOT=$(TESTDATA_LIBRARY) \
	AMBAR_DATA_ROOT=$(TESTDATA_DATA) \
	$(BIN) user add $(USERNAME)

# Build, ship the image over SSH and restart the container on the NAS. Reads the
# host and paths from .env (which stays local), so there is nothing environment-
# specific in the repository. `make deploy-config` skips the image and only
# re-copies docker-compose.yml and .env — the fast path while iterating.
deploy:
	./scripts/deploy.sh

deploy-config:
	./scripts/deploy.sh --config-only

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
