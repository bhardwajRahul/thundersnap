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
export THUNDERBOOT_INCLUDE_NESTED_VM=0
exec ./scripts/build-thunderboot-initramfs.sh "${1:-thunderboot-out/initramfs.cpio}"
