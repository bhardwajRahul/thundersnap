#!/bin/sh
set -eu

out=$(realpath -m "${1:-thunderboot-out/initramfs.cpio}")
source_date_epoch=${SOURCE_DATE_EPOCH:-0}
case "$source_date_epoch" in
''|*[!0-9]*) echo "SOURCE_DATE_EPOCH must be a non-negative integer" >&2; exit 1 ;;
esac
include_nested_vm=${THUNDERBOOT_INCLUDE_NESTED_VM:-1}
case "$include_nested_vm" in
0|1) ;;
*) echo "THUNDERBOOT_INCLUDE_NESTED_VM must be 0 or 1" >&2; exit 1 ;;
esac
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT INT TERM
mkdir -p "$(dirname "$out")" "$root"/bin "$root"/dev/pts "$root"/proc \
  "$root"/sys "$root"/run "$root"/tmp "$root"/etc/ssl/certs \
  "$root"/var/lib/thundersnap "$root"/bootconfig

build() {
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$root/$1" "./cmd/$2"
}
build init thunderboot-init
build bin/thundersnapd thundersnapd
build bin/ts ts
build bin/vshd vshd

if [ ! -x /bin/busybox ]; then
	echo "static /bin/busybox is required (install busybox-static)" >&2
	exit 1
fi
cp /bin/busybox "$root/bin/busybox"
ln -s busybox "$root/bin/cp"

# Install btrfs-progs and its complete dynamic runtime. Keep each dependency at
# its absolute path from ldd: in particular, the ELF interpreter path is fixed
# in the executable and cannot be relocated with PATH or LD_LIBRARY_PATH.
copy_dynamic_binary() {
	src=$1
	dst=$2
	if [ ! -x "$src" ]; then
		echo "required binary $src is missing (install btrfs-progs)" >&2
		exit 1
	fi
	cp "$src" "$root/bin/$dst"
	ldd "$src" | awk '
		/=> \/[^ ]+/ { print $3 }
		$1 ~ /^\// { print $1 }
	' | while IFS= read -r lib; do
		[ -n "$lib" ] || continue
		mkdir -p "$root$(dirname "$lib")"
		cp -L "$lib" "$root$lib"
	done
}
copy_dynamic_binary /usr/bin/btrfs btrfs
copy_dynamic_binary /usr/sbin/mkfs.btrfs mkfs.btrfs
copy_dynamic_binary /usr/sbin/blkid blkid
copy_dynamic_binary /usr/sbin/mdadm mdadm
copy_dynamic_binary /usr/sbin/make-bcache make-bcache
copy_dynamic_binary /usr/sbin/nbd-client nbd-client

cp cmd/thundersnapd/policy.jsonc "$root/bin/thundersnap-policy.jsonc"
if [ ! -r /etc/ssl/certs/ca-certificates.crt ]; then
	echo "required CA bundle /etc/ssl/certs/ca-certificates.crt is missing (install ca-certificates)" >&2
	exit 1
fi
cp /etc/ssl/certs/ca-certificates.crt "$root/etc/ssl/certs/ca-certificates.crt"
if [ "$include_nested_vm" = 1 ]; then
	for artifact in vm/cloud-hypervisor vm/vmlinux; do
		if [ ! -x "$artifact" ]; then
			echo "required nested-VM artifact $artifact is missing" >&2
			echo "run ./scripts/fetch-thunderboot-vm-artifacts.sh first, or set THUNDERBOOT_INCLUDE_NESTED_VM=0" >&2
			exit 1
		fi
	done
	cp vm/cloud-hypervisor "$root/bin/cloud-hypervisor"
	cp vm/vmlinux "$root/bin/vmlinux"
fi

# cpio records mtimes, so normalize every entry before the sorted archive walk.
find "$root" -print0 | xargs -0 touch -h -d "@$source_date_epoch"
(cd "$root" && find . -print0 | sort -z | cpio --reproducible --quiet --null -o --format=newc --owner=0:0) >"$out"
echo "built $out"
