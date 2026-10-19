#!/usr/bin/env bash
# Compila el binario de world dentro de un contenedor de Go: el servidor no necesita tener Go instalado.
# Uso: scripts/build.sh  → deja el binario en .build/world
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_IMAGE="${WORLD_GO_IMAGE:-golang:1.27-alpine}"
VERSION="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)"

mkdir -p "$REPO_DIR/.build"
# El tag 1.27 se mueve con cada parche de seguridad, pero Docker usa la copia local si no se pide:
# sin este pull el servidor seguiría compilando con la versión que bajó la primera vez.
docker pull -q "$GO_IMAGE" >/dev/null || echo "Aviso: no se pudo actualizar $GO_IMAGE; se usa la copia local." >&2
docker run --rm \
  -v "$REPO_DIR":/src \
  -v world-gomod:/go/pkg/mod \
  -v world-gocache:/root/.cache/go-build \
  -w /src \
  -e CGO_ENABLED=0 \
  "$GO_IMAGE" \
  go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /src/.build/world ./cmd/world

echo "Compilado: $REPO_DIR/.build/world ($VERSION, $(docker run --rm "$GO_IMAGE" go version | cut -d" " -f3))"
