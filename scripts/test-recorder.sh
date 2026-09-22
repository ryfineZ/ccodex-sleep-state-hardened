#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
go test -race -count=1 ./internal/modelaudit ./internal/requestrecorder ./cmd/ccodex-request-recorder
if command -v node >/dev/null 2>&1; then
  node --check internal/requestrecorder/web/app.js
fi
