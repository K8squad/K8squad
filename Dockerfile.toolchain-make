# syntax=docker/dockerfile:1
# make toolchain image (ISI-3413). apk make on alpine (4.4.x at authoring).
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: make"
RUN apk add --no-cache make
ENTRYPOINT ["/usr/bin/make"]
