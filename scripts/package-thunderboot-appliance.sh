#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/.." && pwd)
out=$(realpath -m "${1:-$repo/thunderboot-out}")
kernel="$out/Image"
initramfs="$out/initramfs.cpio"
manifest="$out/manifest.json"
archive="$out/thunderboot-appliance-linux-arm64.tar.zst"

for file in "$kernel" "$initramfs" "$out/kernel.config"; do
	if [ ! -r "$file" ]; then
		echo "required appliance artifact is missing: $file" >&2
		exit 1
	fi
done

kernel_description=$(file -b "$kernel")
case "$kernel_description" in
*Linux\ kernel\ ARM64\ boot\ executable\ Image*) ;;
*) echo "Image is not an ARM64 Linux boot image: $kernel_description" >&2; exit 1 ;;
esac

trap 'rm -f "$manifest.tmp.$$" "$archive.tmp.$$"' EXIT INT TERM

revision=$(git -C "$repo" rev-parse HEAD)
if ! git -C "$repo" diff --quiet --ignore-submodules -- || ! git -C "$repo" diff --cached --quiet --ignore-submodules --; then
	revision="$revision-dirty"
fi
kernel_version=$(strings "$kernel" | awk '/Linux version [^ ]+/ { for (i = 1; i <= NF; i++) if ($i == "version") { print $(i + 1); exit } }')
[ -n "$kernel_version" ] || kernel_version=unknown
built_at=$(date -u -d "@${SOURCE_DATE_EPOCH:-0}" +%Y-%m-%dT%H:%M:%SZ)

kernel_sha=$(sha256sum "$kernel" | awk '{print $1}')
kernel_size=$(stat -c %s "$kernel")
initramfs_sha=$(sha256sum "$initramfs" | awk '{print $1}')
initramfs_size=$(stat -c %s "$initramfs")
config_sha=$(sha256sum "$out/kernel.config" | awk '{print $1}')
config_size=$(stat -c %s "$out/kernel.config")

cat >"$manifest.tmp.$$" <<EOF
{
  "schemaVersion": 1,
  "architecture": "arm64",
  "operatingSystem": "linux",
  "thundersnapRevision": "$revision",
  "kernelVersion": "$kernel_version",
  "builtAt": "$built_at",
  "artifacts": {
    "Image": {"sha256": "$kernel_sha", "size": $kernel_size},
    "initramfs.cpio": {"sha256": "$initramfs_sha", "size": $initramfs_size},
    "kernel.config": {"sha256": "$config_sha", "size": $config_size}
  }
}
EOF
mv "$manifest.tmp.$$" "$manifest"

# Stable ownership/order/metadata makes repeated packaging of unchanged inputs
# byte-identical. The uncompressed kernel/initramfs remain next to this archive
# for direct local use.
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@${SOURCE_DATE_EPOCH:-0}" \
	-C "$out" -cf - manifest.json Image initramfs.cpio kernel.config \
	| zstd --quiet --threads=0 -19 -o "$archive.tmp.$$"
mv "$archive.tmp.$$" "$archive"

sha256sum "$manifest" "$kernel" "$initramfs" "$archive"
echo "packaged $archive"
