# syntax=docker/dockerfile:1
# Python toolchain image (ISI-3413). Rebases the official python:3.12-alpine image
# (already minimal, multi-arch, includes pip) so the catalog ref is K8squad-owned,
# signed, and rebuilt weekly for CVEs like every other toolchain image.
ARG PYTHON_IMAGE=python:3.12-alpine
FROM ${PYTHON_IMAGE}
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: python"
ENTRYPOINT ["/usr/local/bin/python3"]
