DOCKER_COMPOSE = docker compose

# Canonical source of whisper.proto.
# For remote fetch (e.g. in CI without access to the backend repo):
#   make proto WHISPER_PROTO_SRC=https://raw.githubusercontent.com/org/transcriber/main/proto/whisper.proto
WHISPER_PROTO_SRC ?= ../../backends/transcriber/proto/whisper.proto

# Install all dev tools: buf + Go protoc plugins.
# buf install: https://buf.build/docs/installation
#   macOS: brew install bufbuild/buf/buf
.PHONY: install
install:
	go install github.com/bufbuild/buf/cmd/buf@v1.67.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

.PHONY: up
up:
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

.PHONY: test
test:
	cd bot && go test ./...

.PHONY: cover
cover:
	cd bot && go test -coverprofile=/tmp/cover.out ./... && go tool cover -func=/tmp/cover.out | grep -v '/gen/'

# Sync proto from the canonical source (backends/transcriber) and regenerate Go stubs.
# Requires: buf, protoc-gen-go, protoc-gen-go-grpc  →  make install
.PHONY: proto
proto:
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
