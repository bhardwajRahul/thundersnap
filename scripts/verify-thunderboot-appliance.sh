#!/bin/sh
set -eu

archive=${1:-thunderboot-out/thunderboot-appliance-linux-arm64.tar.zst}
archive=$(realpath -m "$archive")
[ -r "$archive" ] || { echo "appliance archive not found: $archive" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
zstd --quiet --decompress --stdout "$archive" | tar -C "$work" -xf -

python3 - "$work" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
expected = {"manifest.json", "Image", "initramfs.cpio", "kernel.config"}
actual = {p.name for p in root.iterdir()}
if actual != expected:
    raise SystemExit(f"archive members differ: got {sorted(actual)}, want {sorted(expected)}")
manifest = json.loads((root / "manifest.json").read_text())
if manifest.get("schemaVersion") != 1:
    raise SystemExit(f"unsupported schemaVersion: {manifest.get('schemaVersion')!r}")
if manifest.get("architecture") != "arm64" or manifest.get("operatingSystem") != "linux":
    raise SystemExit("manifest is not a Linux/ARM64 appliance")
artifacts = manifest.get("artifacts")
if not isinstance(artifacts, dict) or set(artifacts) != expected - {"manifest.json"}:
    raise SystemExit("manifest artifact set is incomplete or unexpected")
for name, metadata in artifacts.items():
    data = (root / name).read_bytes()
    size = len(data)
    digest = hashlib.sha256(data).hexdigest()
    if metadata.get("size") != size:
        raise SystemExit(f"{name}: size {size}, manifest says {metadata.get('size')}")
    if metadata.get("sha256") != digest:
        raise SystemExit(f"{name}: sha256 {digest}, manifest says {metadata.get('sha256')}")
print(f"manifest OK: {manifest['thundersnapRevision']} ({manifest['kernelVersion']})")
PY

kernel_description=$(file -b "$work/Image")
case "$kernel_description" in
*Linux\ kernel\ ARM64\ boot\ executable\ Image*) ;;
*) echo "Image is not an ARM64 Linux boot image: $kernel_description" >&2; exit 1 ;;
esac

inspect="$work/initramfs"
mkdir "$inspect"
(cd "$inspect" && cpio -id --quiet <"$work/initramfs.cpio")
elf_list="$work/elf-list"
: >"$elf_list"
find "$inspect" -type f | while IFS= read -r file; do
	description=$(file -b "$file")
	case "$description" in
	*ELF*ARM\ aarch64*) printf '%s\n' "${file#$inspect/}" >>"$elf_list" ;;
	*ELF*) echo "non-ARM64 ELF in initramfs: ${file#$inspect/}: $description" >&2; exit 1 ;;
	esac
done
for required in init bin/thundersnapd bin/thunderboot-logrelay bin/ts bin/vshd bin/busybox bin/btrfs bin/mkfs.btrfs bin/blkid bin/mdadm bin/make-bcache bin/nbd-client; do
	grep -Fqx "$required" "$elf_list" || { echo "required ARM64 executable missing from initramfs: $required" >&2; exit 1; }
done
for omitted in bin/cloud-hypervisor bin/vmlinux; do
	[ ! -e "$inspect/$omitted" ] || { echo "nested-VM payload unexpectedly present: $omitted" >&2; exit 1; }
done

echo "verified $archive"
