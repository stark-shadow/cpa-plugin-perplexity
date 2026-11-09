#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p .build dist

# The race suite must run natively: a cross-compiled binary cannot execute here, so
# the test step is pinned to the host toolchain even when a target GOOS is requested.
# CC must be reset too: the CI matrix exports a cross compiler (mingw / aarch64) for
# the artifact step, and reusing it here makes cgo build host-arch test binaries with
# the wrong compiler, which fails before any test runs.
# Never read CC here: the matrix exports the cross compiler into this process, so
# "go env CC" would echo that value back. macOS and Ubuntu runners both provide cc.
HOST_CC="${HOST_CC:-cc}"
GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" CC="$HOST_CC" CGO_ENABLED=1 \
  go test -race -v ./...

ID="perplexity"
VERSION="$(python3 -c 'import json; print(json.load(open("manifest.json"))["version"])')"
GOOS_TARGET="${GOOS:-$(go env GOHOSTOS)}"
GOARCH_TARGET="${GOARCH:-$(go env GOHOSTARCH)}"
case "$GOOS_TARGET" in
  windows) EXT="dll" ;;
  darwin)  EXT="dylib" ;;
  *)       EXT="so" ;;
esac

if [[ -n "${CPA_CORE:-}" ]]; then
  work="$PWD/.build/go.work"
  core="$(realpath "$CPA_CORE")"
  version="$(awk '$1 == "go" {print $2; exit}' "$core/go.mod")"
  printf 'go %s\nuse (\n "%s"\n "%s"\n)\n' "$version" "$PWD" "$core" > "$work"
  export GOWORK="$work"
else
  unset GOWORK || true
fi

stage="dist/${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}"
rm -rf "$stage"
mkdir -p "$stage"
printf '+ CGO_ENABLED=1 GOOS=%s GOARCH=%s go build -trimpath -buildmode=c-shared -o %s/%s.%s .\n' \
  "$GOOS_TARGET" "$GOARCH_TARGET" "$stage" "$ID" "$EXT"
TARGET_CC="${TARGET_CC:-${CC:-}}"
if [[ -z "$TARGET_CC" && "$GOOS_TARGET" != "$(go env GOHOSTOS)" ]]; then
  case "$GOOS_TARGET-$GOARCH_TARGET" in
    windows-amd64) TARGET_CC="x86_64-w64-mingw32-gcc" ;;
    linux-arm64)   TARGET_CC="aarch64-linux-gnu-gcc" ;;
    *)             TARGET_CC="cc" ;;
  esac
fi
[[ -n "$TARGET_CC" ]] || TARGET_CC="cc"
GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" CC="$TARGET_CC" CGO_ENABLED=1 \
  go build -trimpath -buildmode=c-shared -o "$stage/${ID}.${EXT}" .
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
