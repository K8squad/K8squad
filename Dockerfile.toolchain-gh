# syntax=docker/dockerfile:1
# GitHub CLI toolchain image (ISI-3413). Static upstream release binary, staged on alpine.
FROM alpine:3.21 AS fetch
ARG GH_VERSION=v2.62.0
ARG TARGETARCH
RUN apk add --no-cache curl tar \
 && curl -fsSL "https://github.com/cli/cli/releases/download/${GH_VERSION}/gh_${GH_VERSION#v}_linux_${TARGETARCH}.tar.gz" \
      -o /tmp/gh.tgz \
 && tar -xzf /tmp/gh.tgz -C /tmp \
 && install -m 0755 "/tmp/gh_${GH_VERSION#v}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: gh"
RUN apk add --no-cache ca-certificates git
COPY --from=fetch /usr/local/bin/gh /usr/local/bin/gh
ENTRYPOINT ["/usr/local/bin/gh"]
