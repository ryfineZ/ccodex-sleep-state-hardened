#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:-$ROOT/config/reliable-proxy.json}"
if [[ "$CONFIG" != /* ]]; then CONFIG="$PWD/$CONFIG"; fi
cd "$ROOT"
mkdir -p bin
go build -o bin/ccodex-reliable-proxy ./cmd/ccodex-reliable-proxy
exec ./bin/ccodex-reliable-proxy -config "$CONFIG"
