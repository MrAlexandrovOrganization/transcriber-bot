DOCKER_COMPOSE = docker compose

.PHONY: install
install:
	poetry install

.PHONY: run
run:
	poetry run python main.py

.PHONY: clean
clean:
	find . -type f -name '*.pyc' -delete
	find . -type d -name '__pycache__' -exec rm -rf {} +

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

# Regenerate Python gRPC stubs from proto/whisper.proto
.PHONY: proto
proto:
	poetry run python -m grpc_tools.protoc \
		-I . \
		--python_out=. \
		--grpc_python_out=. \
		proto/whisper.proto

# Regenerate Go gRPC stubs into bot-go/gen/whisper/
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc
# Install plugins: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#                  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
.PHONY: proto-go
proto-go:
	mkdir -p bot-go/gen/whisper
	protoc -I proto \
		--go_out=bot-go/gen/whisper --go_opt=paths=source_relative \
		--go-grpc_out=bot-go/gen/whisper --go-grpc_opt=paths=source_relative \
		proto/whisper.proto
