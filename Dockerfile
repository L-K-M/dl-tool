# syntax=docker/dockerfile:1

# Global defaults keep a bare `docker build` (make docker-build) from stamping
# empty versions or a spec-invalid empty created date; the release workflow
# overrides them via --build-arg (doc 10 section 10). Stages re-declare the
# names bare to inherit these values. Epoch is the reproducible-builds "unset".
ARG VERSION=dev REVISION=unknown CREATED=1970-01-01T00:00:00Z

FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build                     # -> /web/dist, asset URLs relative (vite base: './')

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION REVISION
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /web/dist ./internal/api/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/dl-tool ./cmd/dl-tool

# 7-Zip: fetched on the build platform, selected by TARGETARCH, verified by
# SHA-256 (ADR-0021). Upstream's tarball ships a static 7zzs — the only build
# carrying the RAR codec that runs on musl; Alpine's 7zip package compiles it
# out and the tarball's dynamic 7zz is glibc-linked.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS sevenzip
ARG TARGETARCH
# The three defaults below ARE the pin: the only place the 7-Zip version and
# hashes are recorded. Bumps follow the ADR-0018 cadence.
ARG SEVENZIP_VERSION=2603
ARG SEVENZIP_SHA256_AMD64=dc99eff5008f1ab79bd7084c68513701547a808a89502bf4133683535ab3c695
ARG SEVENZIP_SHA256_ARM64=2389ba20e4d8295e8709c20b6263b69bd1ec4972fe38a04ad7a1badbf595b996
RUN apk add --no-cache xz && \
    set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x64;   sum="${SEVENZIP_SHA256_AMD64}" ;; \
      arm64) arch=arm64; sum="${SEVENZIP_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    wget -q -O /tmp/7z.tar.xz \
      "https://www.7-zip.org/a/7z${SEVENZIP_VERSION}-linux-${arch}.tar.xz"; \
    echo "${sum}  /tmp/7z.tar.xz" | sha256sum -c -; \
    xz -dc /tmp/7z.tar.xz | tar -xf - -C /tmp 7zzs; \
    install -m 0755 /tmp/7zzs /7zz

# yt-dlp: fetched on the build platform, selected by TARGETARCH, verified by SHA-256.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS ytdlp
ARG TARGETARCH
# The four defaults below ARE the pin. They are the only place the yt-dlp version and
# hashes are recorded, and the weekly job in section 10.1 rewrites exactly these lines.
# YTDLP_SHA256_WHEEL pins the py3-none-any wheel scripts/gen-ytdlp-patterns.py
# downloads at maintenance time (ADR-0022); the image itself ships the binary above.
ARG YTDLP_VERSION="2026.08.19"
ARG YTDLP_SHA256_AMD64="f3dec9cfeaf304cec98290fe41c6ad465d4b747d302473559643e7af24929722"
ARG YTDLP_SHA256_ARM64="17b164c4d258be92bb1ad146cb7c336b783aedb380814aabbcb7d52937f77e57"
ARG YTDLP_SHA256_WHEEL="1d57897e94c6665a0a6f9bc54b34e584284e32c034ffab3a7df25d8f7b24eedf"
RUN apk add --no-cache curl
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) file=yt-dlp_musllinux;         sum="${YTDLP_SHA256_AMD64}" ;; \
      arm64) file=yt-dlp_musllinux_aarch64; sum="${YTDLP_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /yt-dlp \
      "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${file}"; \
    echo "${sum}  /yt-dlp" | sha256sum -c -; \
    chmod 0755 /yt-dlp

FROM alpine:3.22
ARG VERSION REVISION CREATED
LABEL org.opencontainers.image.title="dl-tool" \
      org.opencontainers.image.description="Self-hosted download manager: one queue for HTTP, FTP, BitTorrent and media sites" \
      org.opencontainers.image.url="https://github.com/L-K-M/dl-tool" \
      org.opencontainers.image.documentation="https://github.com/L-K-M/dl-tool/tree/main/docs" \
      org.opencontainers.image.source="https://github.com/L-K-M/dl-tool" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.vendor="L-K-M" \
      org.opencontainers.image.licenses="Unlicense AND MIT AND MPL-2.0 AND LGPL-2.1-or-later AND LicenseRef-unRAR" \
      org.opencontainers.image.base.name="docker.io/library/alpine:3.22"
RUN apk add --no-cache su-exec ca-certificates tzdata nodejs
COPY --from=build /out/dl-tool /usr/local/bin/dl-tool
COPY --from=ytdlp /yt-dlp     /usr/local/bin/yt-dlp
COPY --from=sevenzip /7zz /usr/local/bin/7zz
COPY --chmod=755 deploy/entrypoint.sh /entrypoint.sh
ENV PUID=1000 PGID=1000 UMASK=002 TZ=Etc/UTC \
    DLTOOL_HTTP_ADDR=:8080 \
    DLTOOL_CONFIG_DIR=/config \
    DLTOOL_DATA_ROOTS=/data \
    DLTOOL_DB_PATH=/config/dl-tool.db \
    DLTOOL_YTDLP_PATH=/usr/local/bin/yt-dlp \
    DLTOOL_SEVENZIP_PATH=/usr/local/bin/7zz
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/dl-tool", "healthcheck"]
ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
