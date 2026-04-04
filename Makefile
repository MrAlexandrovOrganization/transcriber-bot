DOCKER_COMPOSE = docker compose

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

.PHONY: test
test:
	cd bot && go test ./...

.PHONY: cover
cover:
	cd bot && go test -coverprofile=/tmp/cover.out ./... && go tool cover -func=/tmp/cover.out | grep -v '/gen/'

# Regenerate Go gRPC stubs locally (Docker build does this automatically).
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc
# Install: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#          go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
.PHONY: proto-go
proto-go:
	mkdir -p bot/gen/whisper
	protoc -I proto \
		--go_out=bot/gen/whisper --go_opt=paths=source_relative \
		--go-grpc_out=bot/gen/whisper --go-grpc_opt=paths=source_relative \
		proto/whisper.proto
