#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p build

# Run the full unit test suite on the native Linux build host.
go test -v ./...

# Build the native Linux/amd64 CPA plugin as an ELF shared object.
export CGO_ENABLED=1
export GOOS=linux
export GOARCH=amd64
export GOAMD64=v1
export CGO_CFLAGS="${CGO_CFLAGS:--march=x86-64}"
go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o build/image-param-injector.so .
rm -f build/image-param-injector.h

file build/image-param-injector.so
readelf -h build/image-param-injector.so | grep -E 'Class:|Machine:'
nm -D --defined-only build/image-param-injector.so | grep ' cliproxy_plugin_init$'
glibc_versions="$(readelf --version-info build/image-param-injector.so | sed -n 's/.*Name: GLIBC_\([0-9.]*\).*/\1/p' | sort -Vu)"
if [[ -n "${glibc_versions}" ]]; then
  printf 'Required GLIBC versions:\n%s\n' "${glibc_versions}"
  max_glibc="$(printf '%s\n' "${glibc_versions}" | sort -V | tail -n 1)"
  if [[ "$(printf '2.17\n%s\n' "${max_glibc}" | sort -V | tail -n 1)" != "2.17" ]]; then
    printf 'Linux plugin requires GLIBC_%s; build in manylinux2014 for GLIBC_2.17 compatibility\n' "${max_glibc}" >&2
    exit 1
  fi
fi
sha256sum build/image-param-injector.so
