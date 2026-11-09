#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p build

# Run unit tests on the Linux build host first.
go test ./...

# Build the native Windows/amd64 CPA plugin DLL.
export CGO_ENABLED=1
export GOOS=windows
export GOARCH=amd64
export CC=x86_64-w64-mingw32-gcc
go build -trimpath -buildmode=c-shared -ldflags="-s -w" -o build/image-param-injector.dll .
rm -f build/image-param-injector.h

file build/image-param-injector.dll
sha256sum build/image-param-injector.dll

