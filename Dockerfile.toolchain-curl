# syntax=docker/dockerfile:1
# curl toolchain image (ISI-3413). apk curl on alpine (8.x at authoring).
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: curl"
RUN apk add --no-cache curl ca-certificates
ENTRYPOINT ["/usr/bin/curl"]
