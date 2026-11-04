#!/usr/bin/env bash
# Builds ready-to-run downloads for every OS into dist/.
#   ./scripts/package.sh v0.1.0
set -euo pipefail
version="${1:?usage: $0 <version>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/dist"
rm -rf "$out" && mkdir -p "$out"
cd "$root"
for target in windows/amd64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  name="recura_${version}_${os}_${arch}"
  dir="$out/$name"; mkdir -p "$dir"
  ext=""; [ "$os" = windows ] && ext=".exe"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$dir/recura$ext" ./cmd/recura
  cp examples/config.compose.json "$dir/config.json"
  cp LICENSE "$dir/"
  if [ "$os" = windows ]; then
    printf '@echo off\r\nrem Starts Recura on http://localhost:8080\r\nrem For a real model first run:  set ANTHROPIC_API_KEY=sk-ant-...\r\ncd /d "%%~dp0"\r\nset RECURA_CONFIG=config.json\r\nset RECURA_LOG=text\r\necho Recura is starting. Open http://localhost:8080 in your browser. Press Ctrl+C to stop.\r\nrecura.exe\r\npause\r\n' > "$dir/start-recura.bat"
    (cd "$out" && zip -qr "$name.zip" "$name")
  else
    printf '#!/bin/sh\n# Starts Recura on http://localhost:8080\n# For a real model first run:  export ANTHROPIC_API_KEY=sk-ant-...\ncd "$(dirname "$0")"\necho "Recura is starting. Open http://localhost:8080 in your browser. Press Ctrl+C to stop."\nRECURA_CONFIG=config.json RECURA_LOG=text exec ./recura\n' > "$dir/start-recura.sh"
    chmod +x "$dir/start-recura.sh" "$dir/recura"
    (cd "$out" && tar czf "$name.tar.gz" "$name")
  fi
  rm -rf "$dir"
done
(cd "$out" && sha256sum *.zip *.tar.gz > checksums.txt)
ls -1 "$out"
