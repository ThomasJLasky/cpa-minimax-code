#!/usr/bin/env bash
# 构建插件动态库。默认用本机 GOOS/GOARCH。
#
#   ./scripts/build.sh                     # 本机
#   GOOS=linux GOARCH=arm64 ./scripts/build.sh
#   ./scripts/build.sh --package           # 同时打商店格式的 zip
set -euo pipefail

cd "$(dirname "$0")/.."

PLUGIN_ID="minimax-code"
VERSION="$(grep -o 'pluginVersion = "[^"]*"' dispatch.go | head -1 | cut -d'"' -f2)"
GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"

case "$GOOS" in
  darwin) EXT="dylib" ;;
  windows) EXT="dll" ;;
  *) EXT="so" ;;
esac

mkdir -p dist
OUT="dist/${PLUGIN_ID}.${EXT}"

# 在 Apple Silicon 上交叉编 darwin/amd64：clang 支持 -arch，不需要 Intel 机器。
HOST_GOARCH="$(go env GOARCH)"
if [[ "$GOOS" == "darwin" && "$GOARCH" == "amd64" && "$HOST_GOARCH" == "arm64" ]]; then
  export CC="clang -arch x86_64"
  echo "cross-compiling darwin/amd64 with CC='$CC'"
fi

echo "building ${PLUGIN_ID} v${VERSION} for ${GOOS}/${GOARCH}"
CGO_ENABLED=1 GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -trimpath -buildmode=c-shared -o "$OUT" .

echo "artifact: $OUT"
file "$OUT" 2>/dev/null || true

if [[ "${1:-}" == "--package" ]]; then
  PY="$(command -v python3 || command -v python)"
  "$PY" scripts/package.py --version "$VERSION" --goos "$GOOS" --goarch "$GOARCH" --lib "$OUT"
fi
