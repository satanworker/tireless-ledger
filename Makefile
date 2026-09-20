SOPS_FILE      ?= secrets/pi-memoryd.sops.env
ENV_FILE       ?= .env
BIN_DIR        ?= bin
BIN            ?= $(BIN_DIR)/pi-memoryd
UPLOAD_BIN     ?= $(BIN_DIR)/tireless-upload
INSTALL_DIR    ?= $(HOME)/.local/bin
DATA_DIR       ?= $(HOME)/.local/share/pi-memoryd
PREFIX         ?= $(HOME)/.local
BUILDX_BUILDER ?= tireless-limited
DOCKER_IMAGE   ?= pi-memoryd:local

# Native Mac (or host) build: static-ish, stripped, reproducible paths.
GO_ENV         ?= CGO_ENABLED=0
GO_FLAGS       ?= -trimpath
GO_LDFLAGS     ?= -s -w
GOOS           ?= $(shell go env GOOS)
GOARCH         ?= $(shell go env GOARCH)

.PHONY: all build build-native release test clean \
	install uninstall run secrets-decrypt secrets-edit render-config \
	docker-build up down doctor recall-local embed-local mac-test \
	install-startup-service install-optimize-timer install-server-uploader install-upload-timer \
	build-uploader install-uploader-bin install-mac-uploader install-raw-uploader \
	fragment-counts migrate-split

all: build

## Primary: native host binary (optimized). Mac CLI lives in NixOS/satanworker/pkgs/tireless.
build build-native: $(BIN)

$(BIN): go.mod go.sum $(wildcard *.go)
	@mkdir -p $(BIN_DIR)
	$(GO_ENV) go build $(GO_FLAGS) -ldflags='$(GO_LDFLAGS)' -o $(BIN) .
	@echo "built $(BIN) ($(GOOS)/$(GOARCH), CGO_ENABLED=0, stripped)"

build-uploader: $(UPLOAD_BIN)

$(UPLOAD_BIN): go.mod go.sum $(wildcard cmd/tireless-upload/*.go)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(GO_FLAGS) -ldflags='$(GO_LDFLAGS)' -o $(UPLOAD_BIN) ./cmd/tireless-upload

## Cross / explicit release artifacts
release:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(GO_FLAGS) -ldflags='$(GO_LDFLAGS)' \
		-o $(BIN_DIR)/pi-memoryd-darwin-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(GO_FLAGS) -ldflags='$(GO_LDFLAGS)' \
		-o $(BIN_DIR)/pi-memoryd-darwin-amd64 .
	@echo "release binaries in $(BIN_DIR)/"

test:
	$(GO_ENV) go test ./...

mac-test: test

GGUF ?= $(HOME)/.local/share/tireless-ledger/bge-small-en-v1.5-f16.gguf

## llama.cpp embedder (no Python). Keep off :8080/:8090.
embed-local:
	@test -s $(GGUF) || (echo "missing $(GGUF)" >&2; exit 1)
	llama-server -m $(GGUF) --host 127.0.0.1 --port 8091 --embedding --pooling cls --embeddings -c 512 -b 512 -ub 512

## Local smoke: in-RAM index, no R2 and no CGO.
recall-local: $(BIN)
	@mkdir -p /tmp/pi-memoryd-local
	PI_MEMORYD_STORAGE_URL=memory:// PI_MEMORYD_STATE=/tmp/pi-memoryd-local/dedup.json \
		$(BIN) --listen :8090 --storage-url memory:// --dry-run-s3

clean:
	rm -rf $(BIN_DIR) pi-memoryd

install: $(BIN)
	@mkdir -p $(INSTALL_DIR) $(DATA_DIR)
	install -m 0755 $(BIN) $(INSTALL_DIR)/pi-memoryd
	@echo "installed $(INSTALL_DIR)/pi-memoryd"
	@echo "data dir:  $(DATA_DIR)"

uninstall:
	rm -f $(INSTALL_DIR)/pi-memoryd

secrets-decrypt:
	sops -d $(SOPS_FILE) > $(ENV_FILE)
	chmod 0600 $(ENV_FILE)

secrets-edit:
	sops $(SOPS_FILE)

render-config: secrets-decrypt
	./scripts/render-config.sh $(ENV_FILE)

## Run native daemon with SOPS-backed env (no Docker)
run: $(BIN)
	./scripts/run-native.sh

doctor:
	@echo "go:      $$(go version)"
	@echo "target:  $$(go env GOOS)/$$(go env GOARCH)"
	@echo "sops:    $$(command -v sops || echo MISSING)"
	@echo "binary:  $$(test -x $(BIN) && ls -lh $(BIN) || echo 'not built (make build)')"
	@echo "sops file: $(SOPS_FILE)"

## Optional: Linux/server only. Not the Mac path.
docker-build:
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container \
			--driver-opt memory=3g,memory-swap=4g,cpu-quota=100000,cpu-period=100000
	docker buildx build --builder $(BUILDX_BUILDER) --load --tag $(DOCKER_IMAGE) .
	docker buildx stop $(BUILDX_BUILDER) >/dev/null

up: secrets-decrypt
	docker compose up -d --no-build

down:
	docker compose down

fragment-counts:
	docker compose run --rm --no-deps --no-TTY pi-memoryd --fragment-counts

# Resumable and idempotent: progress is checkpointed in the daemon data volume.
# Production should be dual-writing before this copy begins.
migrate-split:
	docker compose run --rm --no-deps --no-TTY pi-memoryd --migrate-split

install-optimize-timer:
	./scripts/install-optimize-timer.sh

install-startup-service:
	./scripts/install-startup-service.sh

# Fedora/VPS: install the uploader binary and its systemd user timer.
install-server-uploader install-upload-timer:
	./scripts/install-upload-timer.sh

install-uploader-bin: $(UPLOAD_BIN)
	@mkdir -p $(INSTALL_DIR)
	install -m 0755 $(UPLOAD_BIN) $(INSTALL_DIR)/tireless-upload

# macOS: install the uploader binary and its launchd agent.
install-mac-uploader install-raw-uploader: install-uploader-bin
	./scripts/install-raw-upload-launchd.sh
