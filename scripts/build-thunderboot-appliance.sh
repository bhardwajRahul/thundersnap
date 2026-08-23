#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/.." && pwd)

if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != aarch64 ]; then
	echo "the Aperture thunderboot appliance must be built on ARM64 Linux" >&2
	echo "on macOS, run: ./scripts/thunderboot-builder.sh build" >&2
	exit 1
fi

cd "$repo"
version=${THUNDERSNAP_VERSION:-$(./scripts/version.sh)}
[ -n "$version" ] || { echo "THUNDERSNAP_VERSION must not be empty" >&2; exit 1; }
export THUNDERSNAP_VERSION=$version
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
	export SOURCE_DATE_EPOCH
fi
out=$(realpath -m "${1:-thunderboot-out}")
rm -rf "$out"
mkdir -p "$out"

./scripts/build-thunderboot-kernel-arm64.sh "$out/Image"
export THUNDERBOOT_INCLUDE_NESTED_VM=0
./scripts/build-thunderboot-initramfs.sh "$out/initramfs.cpio"
./scripts/package-thunderboot-appliance.sh "$out"
./scripts/verify-thunderboot-appliance.sh "$out/thunderboot-appliance-linux-arm64.tar.zst"
