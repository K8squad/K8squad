# syntax=docker/dockerfile:1
# Go toolchain image (ISI-3413). Full GOROOT staged from the official golang image;
# git is included because `go mod` shells out to it.
ARG GO_IMAGE=golang:1.23-alpine
FROM ${GO_IMAGE} AS src

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: go"
RUN apk add --no-cache git ca-certificates
COPY --from=src /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
ENTRYPOINT ["/usr/local/go/bin/go"]
