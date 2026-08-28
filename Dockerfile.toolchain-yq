# syntax=docker/dockerfile:1
# yq toolchain image (ISI-3413). Static upstream mikefarah/yq release binary.
FROM alpine:3.21 AS fetch
ARG YQ_VERSION=v4.53.6
ARG TARGETARCH
RUN apk add --no-cache curl \
 && curl -fsSL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${TARGETARCH}" \
      -o /usr/local/bin/yq \
 && chmod 0755 /usr/local/bin/yq

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/K8squad/K8squad" \
      org.opencontainers.image.title="k8squad toolchain: yq"
COPY --from=fetch /usr/local/bin/yq /usr/local/bin/yq
ENTRYPOINT ["/usr/local/bin/yq"]
