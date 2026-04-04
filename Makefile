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
	$(DOCKER_COMPOSE) up -d --build
	$(DOCKER_COMPOSE) logs -f

.phony: proto
proto:
	poetry run python -m grpc_tools.protoc -I proto --python_out=proto --grpc_python_out=proto proto/whisper.proto
