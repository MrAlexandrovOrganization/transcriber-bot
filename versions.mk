GO_VERSION := $(shell awk '$$1 == "go" {print $$2; exit}' bot/go.mod)
PROTOC_GEN_GO_VERSION := $(shell awk '$$1 == "google.golang.org/protobuf" {print $$2; exit}' bot/go.mod)
BUF_VERSION := 1.67.0
PROTOC_GEN_GO_GRPC_VERSION := 1.6.1
ALPINE_VERSION := 3.20
POSTGRES_VERSION := 17-alpine
VERSION_VARS := GO_VERSION PROTOC_GEN_GO_VERSION BUF_VERSION PROTOC_GEN_GO_GRPC_VERSION ALPINE_VERSION POSTGRES_VERSION
export $(VERSION_VARS)
