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

FROM alpine:3.22
RUN apk add --no-cache su-exec ca-certificates tzdata 7zip nodejs
COPY --from=build /out/dl-tool /usr/local/bin/dl-tool
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
    DLTOOL_SEVENZIP_PATH=/usr/bin/7zz
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/dl-tool", "healthcheck"]
ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
