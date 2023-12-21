# https://github.com/hadolint/hadolint/issues/861
# hadolint ignore=DL3029
FROM --platform="${BUILDPLATFORM:-}" docker.io/busybox:1.36.1@sha256:6fcbd181125495adab5baffadddcc43345e28b17b6cb719ed02dc2da1796e9dd as builder
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
FROM --platform="${TARGETPLATFORM:-linux/amd64}"  docker.io/alpine:3.19.0@sha256:51b67269f354137895d43f3b3d810bfacd3945438e94dc5ac55fdac340352f48 AS runner

USER 65533

COPY --chown=0:0 --from=builder /app/parca-load /parca-load

CMD ["/parca-load"]
