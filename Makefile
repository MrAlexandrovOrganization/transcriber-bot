DOCKER_COMPOSE = docker compose
include versions.mk
export GOTOOLCHAIN := go$(GO_VERSION)

# Canonical source of whisper.proto.
# For remote fetch (e.g. in CI without access to the backend repo):
#   make proto WHISPER_PROTO_SRC=https://raw.githubusercontent.com/org/transcriber/main/proto/whisper.proto
WHISPER_PROTO_SRC ?= ../../backends/transcriber/proto/whisper.proto

LOCAL_BIN := $(CURDIR)/.bin
export PATH := $(LOCAL_BIN):$(PATH)

# Install all dev tools: buf + Go protoc plugins.
# buf install: https://buf.build/docs/installation
#   macOS: brew install bufbuild/buf/buf
.PHONY: install
install:
	go install github.com/bufbuild/buf/cmd/buf@v$(BUF_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$(PROTOC_GEN_GO_GRPC_VERSION)

# Ensure buf + protoc plugins are available. Install into $(LOCAL_BIN) if missing
# so the build works on hosts (e.g. CI runners) that don't have them preinstalled.
.PHONY: ensure-tools
ensure-tools:
	@mkdir -p $(LOCAL_BIN)
	@command -v buf >/dev/null 2>&1 || GOBIN=$(LOCAL_BIN) go install github.com/bufbuild/buf/cmd/buf@v$(BUF_VERSION)
	@command -v protoc-gen-go >/dev/null 2>&1 || GOBIN=$(LOCAL_BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || GOBIN=$(LOCAL_BIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$(PROTOC_GEN_GO_GRPC_VERSION)

.PHONY: up
up: proto
	$(DOCKER_COMPOSE) up -d --build

.PHONY: down
down:
	$(DOCKER_COMPOSE) down

.PHONY: logs
logs:
	$(DOCKER_COMPOSE) logs -f

.PHONY: restart
restart:
	$(DOCKER_COMPOSE) restart

.PHONY: deploy
deploy:
	$(DOCKER_COMPOSE) up -d --build --no-cache

.PHONY: format
format:
	gofmt -w ./bot

.PHONY: check
check: proto-lint test
	@files="$$(gofmt -l ./bot)"; \
	if test -n "$$files"; then \
		printf '%s\n' "$$files"; \
		exit 1; \
	fi
	cd bot && go vet ./...

.PHONY: test
test:
	cd bot && go test ./...

.PHONY: build
build:
	cd bot && go build ./...

.PHONY: test-race
test-race:
	cd bot && go test -race ./...

# Generate from the checked-in schema, without requiring a neighboring checkout.
.PHONY: proto-gen
proto-gen: ensure-tools
	buf generate

.PHONY: cover
cover:
	cd bot && go test -coverprofile=/tmp/cover.out ./... && go tool cover -func=/tmp/cover.out | grep -v '/gen/'

# Sync proto from the canonical source (backends/transcriber) and regenerate Go stubs.
# Requires: buf, protoc-gen-go, protoc-gen-go-grpc (installed automatically if missing).
.PHONY: proto
proto: ensure-tools
	@echo "Syncing proto from $(WHISPER_PROTO_SRC)..."
	@if echo "$(WHISPER_PROTO_SRC)" | grep -qE "^https?://"; then \
		curl -sSfL "$(WHISPER_PROTO_SRC)" -o proto/whisper.proto; \
	else \
		cp "$(WHISPER_PROTO_SRC)" proto/whisper.proto; \
	fi
	mkdir -p bot/gen/whisper
	buf generate

.PHONY: proto-lint
proto-lint:
	buf lint proto

.PHONY: versions compose-build
versions:
	@$(foreach v,$(VERSION_VARS),printf '%s=%s\n' '$(v)' '$($(v))';)
compose-build:
	$(DOCKER_COMPOSE) build

# CI builds/publishes once; VM activates immutable digests (Python 3 + Docker required).
export IMAGE_COMPOSE_FILE := docker-compose.yml
.PHONY: image-build image-publish image-deploy image-activate
image-build:
	python3 scripts/images.py build
image-publish:
	python3 scripts/images.py publish
image-deploy:
	python3 scripts/images.py deploy
image-activate:
	$(IMAGE_COMPOSE) up -d --no-build --pull never --wait --wait-timeout 180

.PHONY: image-check
image-check:
	python3 -B scripts/test_images.py

.PHONY: image-compose
export IMAGE_ARGS
image-compose:
	python3 scripts/images.py compose

include makefiles/ci-deploy.mk
