# syntax=docker/dockerfile:1
# dtctl toolchain image (ISI-3413). Static upstream dynatrace-oss/dtctl release
# binary. NOTE: the catalog tag (1.0) tracks the upstream 0.x line — upstream has
# not cut a 1.0 release; see images/toolchains/matrix.json.
FROM alpine:3.21 AS fetch
ARG DTCTL_VERSION=v0.38.0
ARG TARGETARCH
RUN apk add --no-cache curl tar \
 && curl -fsSL "https://github.com/dynatrace-oss/dtctl/releases/download/${DTCTL_VERSION}/dtctl_${DTCTL_VERSION#v}_linux_${TARGETARCH}.tar.gz" \
      -o /tmp/dtctl.tgz \
 && tar -xzf /tmp/dtctl.tgz -C /tmp \
 && install -m 0755 /tmp/dtctl /usr/local/bin/dtctl

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: dtctl"
RUN apk add --no-cache ca-certificates
COPY --from=fetch /usr/local/bin/dtctl /usr/local/bin/dtctl
ENTRYPOINT ["/usr/local/bin/dtctl"]
