SOPS_FILE      ?= secrets/pi-memoryd.sops.env
ENV_FILE       ?= .env
BIN_DIR        ?= bin
BIN            ?= $(BIN_DIR)/pi-memoryd
INSTALL_DIR    ?= $(HOME)/.local/bin
DATA_DIR       ?= $(HOME)/.local/share/pi-memoryd
PREFIX         ?= $(HOME)/.local

# Native Mac (or host) build: static-ish, stripped, reproducible paths.
GO_ENV         ?= CGO_ENABLED=0
GO_FLAGS       ?= -trimpath
GO_LDFLAGS     ?= -s -w
GOOS           ?= $(shell go env GOOS)
GOARCH         ?= $(shell go env GOARCH)

.PHONY: all build build-native release test clean \
	install uninstall run secrets-decrypt secrets-edit render-config \
	docker-build up down doctor recall-local embed-local mac-test

all: build

## Primary: native host binary (optimized). Mac CLI lives in NixOS/satanworker/pkgs/tireless.
build build-native: $(BIN)

$(BIN): go.mod go.sum $(wildcard *.go)
	@mkdir -p $(BIN_DIR)
	$(GO_ENV) go build $(GO_FLAGS) -ldflags='$(GO_LDFLAGS)' -o $(BIN) .
	@echo "built $(BIN) ($(GOOS)/$(GOARCH), CGO_ENABLED=0, stripped)"

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

## Local smoke: in-RAM index, no B2, no OpenData binary.
recall-local: $(BIN)
	@mkdir -p /tmp/pi-memoryd-local
	PI_MEMORYD_STORAGE_URL=memory:// PI_MEMORYD_STATE=/tmp/pi-memoryd-local/dedup.json \
		$(BIN) --listen :8090 --storage-url memory:// --dry-run-s3 --flush-seconds 1

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
	@echo "vector:  $$(command -v opendata-vector || command -v vector || echo 'not on PATH (needed if PI_MEMORYD_START_VECTOR=true)')"
	@echo "sops file: $(SOPS_FILE)"

## Optional: Linux/server only. Not the Mac path.
docker-build:
	docker compose build

up: render-config
	docker compose up -d --build

down:
	docker compose down
