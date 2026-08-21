#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/.." && pwd)
vm_dir=${THUNDERSNAP_VM_DIR:-"$repo/vm"}

cloud_hypervisor_version=v49.0
cloud_hypervisor_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$cloud_hypervisor_version/cloud-hypervisor-static"
cloud_hypervisor_sha256=899bfad0113fddae440b03bc8eee6e806dd5188946d33cc06cfdf5e837221677

kernel_version=6.12.8
kernel_url="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$kernel_version.tar.xz"
kernel_sha256=2291da065ca04b715c89ee50362aec3f021a7414bc963f1b56736682c8122979
kernel_config="$repo/vm/kernel.config"

mkdir -p "$vm_dir"
if command -v gcc-14 >/dev/null 2>&1; then
	kernel_cc=gcc-14
else
	kernel_cc=gcc
fi
kernel_make_cc=CC=$kernel_cc

download() {
	url=$1
	sha256=$2
	destination=$3
	tmp="$destination.tmp.$$"
	rm -f "$tmp"
	if ! curl --fail --location --http1.1 --retry 5 --retry-all-errors --output "$tmp" "$url"; then
		rm -f "$tmp"
		return 1
	fi
	if ! echo "$sha256  $tmp" | sha256sum --check -; then
		rm -f "$tmp"
		return 1
	fi
	chmod 0755 "$tmp"
	mv "$tmp" "$destination"
}

if [ ! -x "$vm_dir/cloud-hypervisor" ] || ! echo "$cloud_hypervisor_sha256  $vm_dir/cloud-hypervisor" | sha256sum --check --status; then
	echo "fetching Cloud Hypervisor $cloud_hypervisor_version (x86-64)" >&2
	download "$cloud_hypervisor_url" "$cloud_hypervisor_sha256" "$vm_dir/cloud-hypervisor"
fi

if [ ! -r "$kernel_config" ]; then
	echo "kernel config not found at $kernel_config" >&2
	exit 1
fi
config_sha256=$(sha256sum "$kernel_config" | awk '{print $1}')
compiler=$($kernel_cc -dumpfullversion -dumpversion)
stamp="$vm_dir/.vmlinux-$kernel_version-$config_sha256-gcc-$compiler"
if [ ! -x "$vm_dir/vmlinux" ] || [ ! -e "$stamp" ]; then
	echo "building Linux $kernel_version vmlinux (x86-64)" >&2
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT INT TERM
	download "$kernel_url" "$kernel_sha256" "$work/linux.tar.xz"
	tar -C "$work" -xf "$work/linux.tar.xz"
	cp "$kernel_config" "$work/linux-$kernel_version/.config"
	# Linux 6.12 predates GCC 15's C23 default; prefer GCC 14 when available.
	make -C "$work/linux-$kernel_version" $kernel_make_cc olddefconfig
	make -C "$work/linux-$kernel_version" $kernel_make_cc -j"$(getconf _NPROCESSORS_ONLN)" vmlinux
	install -m 0755 "$work/linux-$kernel_version/vmlinux" "$vm_dir/vmlinux"
	rm -f "$vm_dir"/.vmlinux-*
	: >"$stamp"
	trap - EXIT INT TERM
	rm -rf "$work"
fi

if command -v file >/dev/null 2>&1; then
	file "$vm_dir/cloud-hypervisor" "$vm_dir/vmlinux"
else
	echo "generated $vm_dir/cloud-hypervisor and $vm_dir/vmlinux" >&2
fi
