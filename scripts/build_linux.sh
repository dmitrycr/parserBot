#!/usr/bin/env bash
set -euo pipefail

APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
OUTPUT="${OUTPUT:-$APP_DIR/bin/bot-$GOOS-$GOARCH}"

cd "$APP_DIR"
mkdir -p "$(dirname "$OUTPUT")"

echo "Building $OUTPUT for $GOOS/$GOARCH."
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$OUTPUT" \
  ./cmd/bot

echo "Built: $OUTPUT"
