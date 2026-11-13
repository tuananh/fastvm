GO ?= go
BIN := bin

# Install layout (override PREFIX/DESTDIR for packaging or per-user installs).
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
DESTDIR ?=
INSTALL ?= install

.PHONY: all build install test test-race vet fmt clean start image template-images template-check assets e2e demo netns-test docker-test bpf

all: fmt vet build test

build:
	mkdir -p $(BIN)
	$(GO) build -o $(BIN)/fastvm ./cmd/fastvm
	$(GO) build -o $(BIN)/fastvmd ./cmd/fastvmd
	CGO_ENABLED=0 $(GO) build -o $(BIN)/fastvm-agent ./cmd/fastvm-agent

# Default prefix is /usr/local, so this usually needs sudo. Override with
# PREFIX=$HOME/.local for an unprivileged install, or DESTDIR=... for packaging.
# Binaries must be built first (make build) so install does not compile as root.
install:
	@test -x $(BIN)/fastvm -a -x $(BIN)/fastvmd -a -x $(BIN)/fastvm-agent || \
		{ echo "missing binaries: run 'make build' first"; exit 1; }
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m 0755 $(BIN)/fastvm $(BIN)/fastvmd $(BIN)/fastvm-agent "$(DESTDIR)$(BINDIR)/"

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$($(GO) list -f '{{.Dir}}' ./...)

clean:
	rm -rf $(BIN)

start:
	@test -x $(BIN)/fastvmd || { echo "missing $(BIN)/fastvmd: run 'make build' first"; exit 1; }
	@test "$$(id -u)" -eq 0 || { echo "fastvmd needs root for TAP/bridge/nftables: sudo make start"; exit 1; }
	$(BIN)/fastvmd -config hack/config.yaml

FASTVM_SOCKET ?= /run/fastvm/fastvm.sock
FASTVM ?= $(BIN)/fastvm

# Build hack/Dockerfile.example into a bootable base disk via the running daemon.
image:
	@test -x $(BIN)/fastvm || { echo "missing $(BIN)/fastvm: run 'make build' first"; exit 1; }
	@$(FASTVM) -socket $(FASTVM_SOCKET) version >/dev/null 2>&1 || { echo "fastvmd is not reachable at $(FASTVM_SOCKET); start it with: sudo make start (or pass FASTVM_SOCKET=/path/to/fastvm.sock)"; exit 1; }
	$(FASTVM) -socket $(FASTVM_SOCKET) image build -f hack/Dockerfile.example -t demo:latest hack

# Must stay in sync with internal/template/template.go image tags.
TEMPLATE_VERSION ?= v6
TEMPLATE_NAMES := claude codex copilot

# Shared preflight for template-image builds.
template-check:
	@test -x $(BIN)/fastvm || { echo "missing $(BIN)/fastvm: run 'make build' first"; exit 1; }
	@$(FASTVM) -socket $(FASTVM_SOCKET) version >/dev/null 2>&1 || { echo "fastvmd is not reachable at $(FASTVM_SOCKET); start it with: sudo make start (or pass FASTVM_SOCKET=/path)"; exit 1; }

define build_template_image
	@echo "building fastvm/$(1):$(TEMPLATE_VERSION)"
	$(FASTVM) -socket $(FASTVM_SOCKET) image build -f hack/templates/$(1).Dockerfile -t fastvm/$(1):$(TEMPLATE_VERSION) hack/templates
endef

# Build all three images.
template-images: template-check
	@for t in $(TEMPLATE_NAMES); do \
		echo "building fastvm/$$t:$(TEMPLATE_VERSION)"; \
		$(FASTVM) -socket $(FASTVM_SOCKET) image build -f hack/templates/$$t.Dockerfile -t fastvm/$$t:$(TEMPLATE_VERSION) hack/templates || exit $$?; \
	done

# Build one image, e.g. `sudo make template:copilot` (escaped colon target).
template\:claude: template-check
	$(call build_template_image,claude)
template\:codex: template-check
	$(call build_template_image,codex)
template\:copilot: template-check
	$(call build_template_image,copilot)

assets:
	./test/e2e/fetch-assets.sh

demo:
	@test "$$(id -u)" -eq 0 || { echo "make demo must run as root: sudo make demo"; exit 1; }
	./hack/demo-wake.sh

e2e: assets
	FASTVM_E2E=1 $(GO) test -count=1 -timeout 10m ./test/e2e/... ./internal/vmm/firecracker/...

netns-test:
	FASTVM_NET_TESTS=1 $(GO) test -count=1 -timeout 5m ./internal/network/...

docker-test:
	FASTVM_DOCKER_TESTS=1 $(GO) test -count=1 -timeout 10m ./internal/image/...

bpf:
	$(MAKE) -C bpf
