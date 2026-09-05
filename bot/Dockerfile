FROM golang:1.26.1-alpine AS builder

RUN apk add --no-cache protobuf-dev && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

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


FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=builder /bot /bot
CMD ["/bot"]
