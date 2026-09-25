#!/bin/sh
# Cross-compile aif-connect into a fresh dist/ plus SHA256SUMS. POSIX sh: runs in the alpine builder.
#   scripts/dist.sh [GOOS/GOARCH ...]    default: linux/amd64 windows/amd64 darwin/arm64
# The build id comes from $GIT_SHA (the Docker build arg), else git, else "dev".
set -eu
cd "$(dirname "$0")/.."
[ $# -gt 0 ] || set -- linux/amd64 windows/amd64 darwin/arm64
sha=${GIT_SHA:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}
rm -rf dist
mkdir dist
for target in "$@"; do
	os=${target%/*}
	arch=${target#*/}
	ext=
	[ "$os" = windows ] && ext=.exe
	out=dist/aif-connect-$os-$arch$ext
	echo "building $out ($sha)"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X github.com/dshein-alt/aif/internal/version.BuildID=$sha" \
		-o "$out" ./cmd/aif-connect
done
cd dist
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum aif-connect-* >SHA256SUMS
else
	shasum -a 256 aif-connect-* >SHA256SUMS
fi
cat SHA256SUMS
