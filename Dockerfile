# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${BUILDPLATFORM:-}" docker.io/busybox:1.37.0@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74 as builder
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
FROM --platform="${TARGETPLATFORM:-linux/amd64}"  docker.io/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 AS runner

USER 65533

COPY --chown=0:0 --from=builder /app/parca-load /parca-load

CMD ["/parca-load"]
