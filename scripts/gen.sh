#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p api web/src/api
openapi_tmp=$(mktemp "$(pwd)/api/openapi.json.XXXXXX")
trap 'rm -f "$openapi_tmp"' EXIT
go run ./cmd/dl-tool openapi > "$openapi_tmp"
mv "$openapi_tmp" api/openapi.json
cd web
./node_modules/.bin/openapi-typescript ../api/openapi.json -o src/api/schema.d.ts
./node_modules/.bin/prettier --write src/api/schema.d.ts
