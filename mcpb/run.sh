#!/bin/sh
# Запуск sap-notes на macOS и Linux из пакета .mcpb: выбор бинарника по платформе.
DIR="$(cd "$(dirname "$0")" && pwd)"
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  BIN="$DIR/sap-notes-darwin-arm64" ;;
  Darwin-x86_64) BIN="$DIR/sap-notes-darwin-amd64" ;;
  Linux-x86_64)  BIN="$DIR/sap-notes-linux-amd64" ;;
  *) echo "sap-notes: unsupported platform $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac
chmod +x "$BIN" 2>/dev/null
# неподписанный скачанный файл macOS иначе не запустит
[ "$(uname -s)" = "Darwin" ] && xattr -d com.apple.quarantine "$BIN" 2>/dev/null
exec "$BIN" "$@"
