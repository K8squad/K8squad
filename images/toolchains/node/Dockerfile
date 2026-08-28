# syntax=docker/dockerfile:1
# Node.js toolchain image (ISI-3413). node + npm/npx staged from the official
# node-alpine image (musl build — runs on the same alpine runtime base).
ARG NODE_IMAGE=node:22-alpine
FROM ${NODE_IMAGE} AS src

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: node"
RUN apk add --no-cache libstdc++ ca-certificates
COPY --from=src /usr/local/bin/node /usr/local/bin/node
COPY --from=src /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
ENTRYPOINT ["/usr/local/bin/node"]
