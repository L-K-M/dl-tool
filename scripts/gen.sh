#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/dl-tool openapi > api/openapi.json
cd web && npx openapi-typescript ../api/openapi.json -o src/api/schema.d.ts
