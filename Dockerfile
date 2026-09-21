# syntax=docker/dockerfile:1

FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build                     # -> /web/dist, asset URLs relative (vite base: './')

FROM golang:1.26-alpine AS build
ARG VERSION=dev REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /web/dist ./internal/api/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
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
    tar -xJf /tmp/7z.tar.xz -C /tmp 7zzs; \
    install -m 0755 /tmp/7zzs /7zz

FROM alpine:3.22
RUN apk add --no-cache su-exec ca-certificates tzdata nodejs
COPY --from=build /out/dl-tool /usr/local/bin/dl-tool
COPY --from=sevenzip /7zz /usr/local/bin/7zz
COPY --chmod=755 deploy/entrypoint.sh /entrypoint.sh
# NOTE: yt-dlp is NOT installed in this image yet; T093 adds the pinned,
# SHA-256-verified fetch. This ENV only pre-declares where T093 will place it,
# so yt-dlp jobs fail until that task lands.
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
