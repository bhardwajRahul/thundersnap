#!/bin/sh
set -eu

out=$(realpath -m "${1:-thunderboot-out/initramfs.cpio}")
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT INT TERM
mkdir -p "$(dirname "$out")" "$root"/bin "$root"/dev/pts "$root"/proc \
  "$root"/sys "$root"/run "$root"/tmp "$root"/etc \
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

cp cmd/thundersnapd/policy.jsonc "$root/bin/thundersnap-policy.jsonc"
cp vm/cloud-hypervisor "$root/bin/cloud-hypervisor"
cp vm/vmlinux "$root/bin/vmlinux"

(cd "$root" && find . -print0 | sort -z | cpio --quiet --null -o --format=newc --owner=0:0) >"$out"
echo "built $out"
