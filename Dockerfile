# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${BUILDPLATFORM:-}" docker.io/busybox:1.36.1@sha256:82742949a3709938cbeb9cec79f5eaf3e48b255389f2dcedf2de29ef96fd841c as builder
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
FROM --platform="${TARGETPLATFORM:-linux/amd64}"  docker.io/alpine:3.20.2@sha256:0a4eaa0eecf5f8c050e5bba433f58c052be7587ee8af3e8b3910ef9ab5fbe9f5 AS runner

USER 65533

COPY --chown=0:0 --from=builder /app/parca-load /parca-load

CMD ["/parca-load"]
