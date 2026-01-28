# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${BUILDPLATFORM:-}" docker.io/busybox:1.37.0@sha256:2383baad1860bbe9d8a7a843775048fd07d8afe292b94bd876df64a69aae7cb1 as builder
RUN mkdir /.cache && touch -t 202101010000.00 /.cache

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT

WORKDIR /app
COPY dist dist

# NOTICE: See goreleaser.yml for the build paths.
RUN if [ "${TARGETARCH}" = 'amd64' ]; then \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}_${TARGETVARIANT:-v1}/parca-load" . ; \
    elif [ "${TARGETARCH}" = 'arm64' ]; then \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}_${TARGETVARIANT:-v8.0}/parca-load" . ; \
    elif [ "${TARGETARCH}" = 'arm' ]; then \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}_${TARGETVARIANT##v}/parca-load" . ; \
    else \
        cp "dist/parca-load_${TARGETOS}_${TARGETARCH}/parca-load" . ; \
    fi
RUN chmod +x parca-load

# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${TARGETPLATFORM:-linux/amd64}"  docker.io/alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659 AS runner

USER 65533

COPY --chown=0:0 --from=builder /app/parca-load /parca-load

CMD ["/parca-load"]
