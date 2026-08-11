#!/bin/sh
set -eu

out=$(realpath -m "${1:-thunderboot-out/initramfs.cpio}")
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT INT TERM
mkdir -p "$(dirname "$out")" "$root"/dev/pts "$root"/proc "$root"/sys \
  "$root"/run "$root"/tmp "$root"/etc "$root"/sbin \
  "$root"/var/lib/thundersnap "$root"/bootconfig "$root"/vm

build() {
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$root/$1" "./cmd/$2"
}
build init thunderboot-init
build sbin/thundersnapd thundersnapd
build sbin/ts ts
build sbin/vshd vshd
cp cmd/thundersnapd/policy.jsonc "$root/etc/thundersnap-policy.jsonc"
cp vm/cloud-hypervisor "$root/vm/cloud-hypervisor"
cp vm/vmlinux "$root/vm/vmlinux"

(cd "$root" && find . -print0 | sort -z | cpio --quiet --null -o --format=newc --owner=0:0) >"$out"
echo "built $out"
