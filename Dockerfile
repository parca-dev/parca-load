# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${BUILDPLATFORM:-}" docker.io/busybox:1.37.0@sha256:2f590fc602ce325cbff2ccfc39499014d039546dc400ef8bbf5c6ffb860632e7 as builder
RUN mkdir /.cache && touch -t 202101010000.00 /.cache

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT

WORKDIR /app
COPY dist dist

# NOTICE: See goreleaser.yml for the build paths.
RUN if [ "${TARGETARCH}" = 'amd64' ]; then \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}_${TARGETVARIANT:-v1}/parca-load" . ; \
    elif [ "${TARGETARCH}" = 'arm' ]; then \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}_${TARGETVARIANT##v}/parca-load" . ; \
    else \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}/parca-load" . ; \
    fi
RUN chmod +x parca-load

# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${TARGETPLATFORM:-linux/amd64}"  docker.io/alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS runner

USER 65533

COPY --chown=0:0 --from=builder /app/parca-load /parca-load

CMD ["/parca-load"]
