# syntax=docker/dockerfile:1
# uv toolchain image (ISI-3413). Static /uv + /uvx binaries staged from the official
# astral-sh image (scratch-based upstream — we land them on alpine for a shell).
ARG UV_VERSION=0.5.31
FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: uv"
RUN apk add --no-cache ca-certificates
COPY --from=uv /uv /uvx /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/uv"]
