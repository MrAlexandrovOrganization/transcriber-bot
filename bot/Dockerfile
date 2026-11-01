# check=skip=InvalidDefaultArgInFrom
ARG GO_VERSION
ARG ALPINE_VERSION
FROM golang:${GO_VERSION}-alpine AS builder
ARG PROTOC_GEN_GO_VERSION
ARG PROTOC_GEN_GO_GRPC_VERSION
RUN apk add --no-cache make git ca-certificates
WORKDIR /build
COPY bot/go.mod bot/go.sum bot/
RUN cd bot && go mod download
COPY Makefile buf.gen.yaml ./
COPY makefiles/ makefiles/
COPY proto/ proto/
COPY bot/ bot/
RUN make install && CGO_ENABLED=0 GOOS=linux make build-bin

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/.build/bot /bot
CMD ["/bot"]
