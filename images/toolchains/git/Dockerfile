# syntax=docker/dockerfile:1
# git toolchain image (ISI-3413). apk git on alpine — tracks the catalog minor (>= 2.45).
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: git"
RUN apk add --no-cache git ca-certificates
ENTRYPOINT ["/usr/bin/git"]
