DOCKER_COMPOSE_BASE = docker compose --project-directory "$(CURDIR)"
include makefiles/compose.mk
DOCKER_COMPOSE = docker compose --project-directory "$(CURDIR)" $(COMPOSE_FLAGS)
include makefiles/versions.mk
export GOTOOLCHAIN := go$(GO_VERSION)

LOCAL_BIN := $(CURDIR)/.bin
export PATH := $(LOCAL_BIN):$(PATH)

# Install all dev tools: buf + Go protoc plugins.
# buf install: https://buf.build/docs/installation
#   macOS: brew install bufbuild/buf/buf
.PHONY: install
install:
	cd bot && go mod download
	@mkdir -p $(LOCAL_BIN)
	GOBIN=$(LOCAL_BIN) go install github.com/bufbuild/buf/cmd/buf@v$(BUF_VERSION)
	GOBIN=$(LOCAL_BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(LOCAL_BIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$(PROTOC_GEN_GO_GRPC_VERSION)

# Bootstrapping is explicit; CI and Docker install the same pinned tools.
.PHONY: ensure-tools
ensure-tools:
	@command -v buf >/dev/null && command -v protoc-gen-go >/dev/null && command -v protoc-gen-go-grpc >/dev/null || { echo 'Run make install'; exit 1; }

.PHONY: up
up: proto-gen
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
	buf format -w proto

.PHONY: check
check: proto-gen proto-check test
	@files="$$(gofmt -l ./bot)"; \
	if test -n "$$files"; then \
		printf '%s\n' "$$files"; \
		exit 1; \
	fi
	cd bot && go vet ./...

.PHONY: test
test: proto-gen
	cd bot && go test ./...

.PHONY: build
build: proto-gen
	cd bot && go build ./...

.PHONY: test-race
test-race: proto-gen
	cd bot && go test -race ./...

# Generate from the checked-in schema, without requiring a neighboring checkout.
.PHONY: proto-gen
proto-gen: ensure-tools
	buf generate

.PHONY: cover
cover:
	cd bot && go test -coverprofile=/tmp/cover.out ./... && go tool cover -func=/tmp/cover.out | grep -v '/gen/'

# Generation is offline from the pinned snapshot; syncing is a separate action.
.PHONY: proto proto-sync proto-check proto-lint
proto: proto-gen
proto-sync:
	python3 scripts/proto_snapshot.py sync
proto-check:
	python3 scripts/proto_snapshot.py check
	buf format --diff --exit-code proto
	buf build proto -o /dev/null
	buf lint proto
proto-lint: proto-check

.PHONY: versions compose-build
versions:
	@$(foreach v,$(VERSION_VARS),printf '%s=%s\n' '$(v)' '$($(v))';)
compose-build:
	$(DOCKER_COMPOSE) build

# CI builds/publishes once; VM activates immutable digests (Python 3 + Docker required).
export IMAGE_COMPOSE_FILE := compose/docker-compose.yml
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

# Read-only validation of every tracked Compose combination, without runtime secrets.
.PHONY: compose-layout-check
compose-layout-check:
	$(DOCKER_COMPOSE_BASE) --env-file /dev/null -f compose/docker-compose.yml --profile '*' config --no-interpolate --no-env-resolution --quiet

check: compose-layout-check

# Explicit schema application uses the same migrations as startup.
.PHONY: migrate run build-bin test-integration test-db-up test-db-down
migrate:
	cd bot && go run . migrate
run: proto-gen
	cd bot && go run .
build-bin: proto-gen
	cd bot && go build -o ../.build/bot .
test-integration: proto-gen
	@test -n "$(TRANSCRIBER_BOT_TEST_DATABASE_URL)" || (echo 'Set TRANSCRIBER_BOT_TEST_DATABASE_URL to an isolated database'; exit 1)
	cd bot && go test ./store ./bot

test-db-up:
	docker run --rm -d --name transcriber-bot-ci-test-db --label purpose=transcriber-bot-ci-test-db \
		-e POSTGRES_USER=pilot -e POSTGRES_PASSWORD=synthetic-ci-password -e POSTGRES_DB=transcriber_bot_test \
		-p 127.0.0.1:55440:5432 --tmpfs /var/lib/postgresql/data:rw postgres:$(POSTGRES_VERSION)
	@for attempt in $$(seq 1 30); do \
		docker exec transcriber-bot-ci-test-db pg_isready -U pilot -d transcriber_bot_test >/dev/null 2>&1 && exit 0; \
		sleep 1; \
	done; exit 1

test-db-down:
	@if [ "$$(docker inspect --format '{{ index .Config.Labels "purpose" }}' transcriber-bot-ci-test-db 2>/dev/null)" = transcriber-bot-ci-test-db ]; then \
		docker stop transcriber-bot-ci-test-db; \
	fi

include makefiles/observability.mk
