#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"
if [ ! -x "$ROOT/bin/ccodex-request-recorder" ]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "这是源码目录，缺少可执行文件。请使用发布包，或安装 Go 后再启动。"
    read -r _unused
    exit 1
  fi
  mkdir -p bin
  go build -trimpath -o bin/ccodex-request-recorder ./cmd/ccodex-request-recorder
fi
exec "$ROOT/bin/ccodex-request-recorder" setup "$@"
