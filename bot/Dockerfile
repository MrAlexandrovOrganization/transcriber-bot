# check=skip=InvalidDefaultArgInFrom

# Required version arguments are supplied by Make/Compose, without fallback pins.
ARG GO_VERSION
ARG ALPINE_VERSION
FROM golang:${GO_VERSION}-alpine AS builder
ARG PROTOC_GEN_GO_VERSION
ARG PROTOC_GEN_GO_GRPC_VERSION

RUN apk add --no-cache protobuf-dev && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v${PROTOC_GEN_GO_GRPC_VERSION}

WORKDIR /build

COPY bot/go.mod bot/go.sum ./
RUN go mod download

COPY proto/ /proto/
COPY bot/ .

RUN mkdir -p gen/whisper && \
    protoc -I /proto \
        --go_out=gen/whisper --go_opt=paths=source_relative \
        --go-grpc_out=gen/whisper --go-grpc_opt=paths=source_relative \
        /proto/whisper.proto

RUN CGO_ENABLED=0 GOOS=linux go build -o /bot .


FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates
COPY --from=builder /bot /bot
CMD ["/bot"]
