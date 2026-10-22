# syntax=docker/dockerfile:1
# ── Stage 1: build the Go binary ──────────────────────────────────────────────
FROM golang:1.26-trixie AS builder

# The official golang image sets GOTOOLCHAIN=local. The nightly security job can raise the
# go directive past the image's patch release; let the build fetch that toolchain instead.
ENV GOTOOLCHAIN=auto

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download -x

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" \
      -o tv-patcher .

# ── Stage 2: fetch the tools the patcher shells out to ───────────────────────
# Morphe and apkeep are rebuilt at their latest release, like cloudflared in Lantern. CI passes
# the resolved versions (the Morphe jar's file name carries its version) and a date that busts
# the layer cache so the weekly rebuild really refreshes them.
FROM debian:trixie-slim AS tools
ARG TARGETARCH
ARG MORPHE_VERSION
ARG APKEEP_VERSION
ARG TOOLS_REFRESH=unset
RUN apt-get update -qq && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN echo "tools refresh: ${TOOLS_REFRESH}" && test -n "${MORPHE_VERSION}" && test -n "${APKEEP_VERSION}" && \
    curl -fsSL -o /morphe.jar \
      "https://github.com/MorpheApp/morphe-cli/releases/download/${MORPHE_VERSION}/morphe-desktop-${MORPHE_VERSION#v}-all.jar" && \
    case "${TARGETARCH}" in \
      amd64) t=x86_64-unknown-linux-gnu ;; \
      arm64) t=aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported arch ${TARGETARCH}"; exit 1 ;; \
    esac && \
    curl -fsSL -o /apkeep "https://github.com/EFForg/apkeep/releases/download/${APKEEP_VERSION}/apkeep-${t}" && \
    chmod +x /apkeep

# ── Stage 3: runtime ──────────────────────────────────────────────────────────
# Morphe needs a Java runtime, adb and apksigner come from apt, so this is not distroless.
FROM eclipse-temurin:21-jre-noble

RUN apt-get update -qq && apt-get install -y --no-install-recommends \
      android-tools-adb apksigner ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root user (UID 65532, like the Chainguard images used elsewhere). HOME lives in the data
# volume so adb's pairing keys survive container re-creation. Morphe keeps its own data next to
# the jar (/opt/morphe/morphe-data), which must be writable.
RUN groupadd --gid 65532 nonroot && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /data/home nonroot && \
    mkdir -p /opt/morphe/morphe-data /data/home/.android && \
    chown -R 65532:65532 /opt/morphe /data

COPY --from=tools --chown=65532:65532 /morphe.jar /opt/morphe/morphe.jar
COPY --from=tools /apkeep /usr/local/bin/apkeep
COPY --from=builder /build/tv-patcher /usr/local/bin/tv-patcher

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.title="TV Patcher" \
      org.opencontainers.image.description="Keeps Morphe-patched apps installed on Google TVs" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.source="https://github.com/sloccy/morphe-tv-patcher" \
      org.opencontainers.image.licenses="MIT"

ENV HOME=/data/home DATA_DIR=/data MORPHE_JAR=/opt/morphe/morphe.jar
USER 65532:65532
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8093

# The binary probes itself (no shell needed). Do not override this with a compose healthcheck.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/tv-patcher", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tv-patcher"]
CMD ["run"]
