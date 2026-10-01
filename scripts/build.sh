#!/usr/bin/env bash
# Compila el binario de world dentro de un contenedor de Go: el servidor no necesita tener Go instalado.
# Uso: scripts/build.sh  → deja el binario en .build/world
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_IMAGE="${WORLD_GO_IMAGE:-golang:1.27-alpine}"
VERSION="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)"

mkdir -p "$REPO_DIR/.build"
docker run --rm \
  -v "$REPO_DIR":/src \
  -v world-gomod:/go/pkg/mod \
  -v world-gocache:/root/.cache/go-build \
  -w /src \
  -e CGO_ENABLED=0 \
  "$GO_IMAGE" \
  go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /src/.build/world ./cmd/world

echo "Compilado: $REPO_DIR/.build/world ($VERSION)"
