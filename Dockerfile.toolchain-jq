# syntax=docker/dockerfile:1
# jq toolchain image (ISI-3413). apk jq on alpine (1.7.1 at authoring).
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: jq"
RUN apk add --no-cache jq
ENTRYPOINT ["/usr/bin/jq"]
