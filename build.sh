#!/usr/bin/env bash
# Build sap-notes: tests, binaries in dist/, Claude Desktop extension (.mcpb).
# Run: bash build.sh   (on Windows: from Git Bash; the .mcpb step needs zip)
set -euo pipefail
cd "$(dirname "$0")"

NAME=sap-notes
VERSION=$(sed -n 's/.*serverVersion *= *"\(.*\)".*/\1/p' main.go)
echo "== $NAME $VERSION"

# the version lives in three places and they must agree
grep -q "\"version\": \"$VERSION\"" mcpb/manifest.json || { echo "mcpb/manifest.json: version is not $VERSION" >&2; exit 1; }
grep -q "^## \[$VERSION\]" CHANGELOG.md || { echo "CHANGELOG.md: no section for $VERSION" >&2; exit 1; }

go vet ./...
go test ./...

rm -rf dist && mkdir -p dist
export CGO_ENABLED=0
build() { GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "-s -w" -o "dist/$3" . ; echo "  dist/$3"; }
build windows amd64 "$NAME.exe"
build darwin  arm64 "$NAME-darwin-arm64"
build darwin  amd64 "$NAME-darwin-amd64"
build linux   amd64 "$NAME-linux-amd64"

if ! command -v zip >/dev/null; then
  echo "== zip not found — .mcpb skipped; binaries are in dist/"
  exit 0
fi

# .mcpb = zip with manifest.json and server/
STAGE=$(mktemp -d)
mkdir -p "$STAGE/server"
cp mcpb/manifest.json "$STAGE/"
cp mcpb/run.sh dist/$NAME.exe dist/$NAME-darwin-* dist/$NAME-linux-amd64 "$STAGE/server/"
chmod +x "$STAGE/server/"*
(cd "$STAGE" && zip -qr - .) > "dist/$NAME.mcpb"
rm -rf "$STAGE"
echo "  dist/$NAME.mcpb"
