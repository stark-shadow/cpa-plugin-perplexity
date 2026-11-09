#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

ID="perplexity"
VERSION="$(python3 -c 'import json; print(json.load(open("manifest.json"))["version"])')"
GOOS_TARGET="$(go env GOOS)"
GOARCH_TARGET="$(go env GOARCH)"
mkdir -p .build dist

if [[ -n "${CPA_CORE:-}" ]]; then
  work="$PWD/.build/go.work"
  core="$(realpath "$CPA_CORE")"
  version="$(awk '$1 == "go" {print $2; exit}' "$core/go.mod")"
  printf 'go %s\nuse (\n "%s"\n "%s"\n)\n' "$version" "$PWD" "$core" > "$work"
  export GOWORK="$work"
else
  unset GOWORK || true
fi

GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" CC="${HOST_CC:-cc}" CGO_ENABLED=1 go test -race -v ./...

stage="dist/${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}"
rm -rf "$stage"
mkdir -p "$stage"
printf '+ CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o %s/%s.so .\n' "$stage" "$ID"
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$stage/${ID}.so" .
rm -f "$stage/${ID}.h"
cp -f manifest.json README.md README.zh-CN.md LICENSE "$stage/"
python3 package.py "$stage" "dist/${ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip"
mkdir -p dist/artifacts
cp -f "dist/${ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip" dist/artifacts/
(
  cd dist/artifacts
  if command -v sha256sum >/dev/null 2>&1; then
    for f in ./*.zip; do sha256sum "$f" | sed "s|  \./|  |"; done > checksums.txt
  else
    for f in ./*.zip; do shasum -a 256 "$f" | sed "s|  \./|  |"; done > checksums.txt
  fi
  cat checksums.txt
)
