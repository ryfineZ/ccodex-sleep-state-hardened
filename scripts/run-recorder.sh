#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
mkdir -p bin
go build -trimpath -o bin/ccodex-request-recorder ./cmd/ccodex-request-recorder
exec ./bin/ccodex-request-recorder "$@"
