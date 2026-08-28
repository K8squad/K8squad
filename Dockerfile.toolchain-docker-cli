# syntax=docker/dockerfile:1
# docker CLI toolchain image (ISI-3413). CLI ONLY — no daemon; the daemon stays the
# existing dockerd sidecar (values.yaml tools comment). Rebases the official
# docker:27-cli variant so the ref is K8squad-owned, signed, and CVE-rebuilt.
ARG DOCKER_IMAGE=docker:27-cli
FROM ${DOCKER_IMAGE}
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: docker-cli"
ENTRYPOINT ["/usr/local/bin/docker"]
