#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/.." && pwd)

if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != aarch64 ]; then
	echo "the Aperture thunderboot kernel must be built on ARM64 Linux" >&2
	echo "on macOS, run: ./scripts/thunderboot-builder.sh build" >&2
	exit 1
fi

kernel_version=6.12.8
kernel_url="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$kernel_version.tar.xz"
kernel_sha256=2291da065ca04b715c89ee50362aec3f021a7414bc963f1b56736682c8122979
common_config="$repo/vm/kernel.config"
fragment="$repo/build/thunderboot/kernel-arm64.fragment"
out=$(realpath -m "${1:-$repo/thunderboot-out/Image}")
cache=${THUNDERBOOT_KERNEL_CACHE:-"${XDG_CACHE_HOME:-$HOME/.cache}/thunderboot/kernel-$kernel_version-arm64"}
source="$cache/linux-$kernel_version"
archive="$cache/linux-$kernel_version.tar.xz"

mkdir -p "$cache" "$(dirname "$out")"
if [ ! -r "$archive" ] || ! echo "$kernel_sha256  $archive" | sha256sum --check --status; then
	tmp="$archive.tmp.$$"
	rm -f "$tmp"
	trap 'rm -f "$tmp"' EXIT INT TERM
	curl --fail --location --http1.1 --retry 5 --retry-all-errors --output "$tmp" "$kernel_url"
	echo "$kernel_sha256  $tmp" | sha256sum --check -
	mv "$tmp" "$archive"
	trap - EXIT INT TERM
fi
if [ ! -d "$source" ]; then
	tmp_source="$source.tmp.$$"
	rm -rf "$tmp_source"
	mkdir -p "$tmp_source"
	tar -C "$tmp_source" --strip-components=1 -xf "$archive"
	mv "$tmp_source" "$source"
fi

build="$cache/build"
rm -rf "$build"
mkdir -p "$build"

# Start from an empty ARM64 configuration, then merge the common feature set
# from vm/kernel.config and the explicit Apple VZ hardware requirements. Feed
# the merged file through allnoconfig before olddefconfig: this prevents x86-only
# and unrelated hardware choices from surviving the architecture conversion.
: >"$build/.config"
"$source/scripts/kconfig/merge_config.sh" -m -O "$build" \
	"$build/.config" "$common_config" "$fragment"
cp "$build/.config" "$build/all.config"
KCONFIG_ALLCONFIG="$build/all.config" make -C "$source" O="$build" ARCH=arm64 allnoconfig
make -C "$source" O="$build" ARCH=arm64 olddefconfig

# Fail rather than silently accepting a renamed or dependency-disabled ARM64
# hardware option. The common config is the x86 Cloud Hypervisor config and
# intentionally contains architecture-specific symbols that do not exist on
# ARM64, so it is used as a feature source rather than checked line-for-line.
for required_fragment in "$fragment"; do
	while IFS= read -r line; do
		case "$line" in
		CONFIG_*=n)
			key=${line%%=*}
			required="# $key is not set"
			# Kconfig omits a disabled symbol when its parent menu is
			# disabled. Treat that as n, but reject an enabled value.
			if ! grep -Fqx "$required" "$build/.config" &&
				! grep -Eq "^${key}=" "$build/.config"; then
				continue
			fi
			;;
		CONFIG_*=y|CONFIG_*='"'*'"')
			key=${line%%=*}
			required=$line
			;;
		*) continue ;;
		esac
		if ! grep -Fqx "$required" "$build/.config"; then
			echo "kernel configuration did not preserve required setting: $line" >&2
			grep -E "^${key}=|^# ${key} is not set" "$build/.config" >&2 || true
			exit 1
		fi
	done <"$required_fragment"
done

jobs=${THUNDERBOOT_KERNEL_JOBS:-$(getconf _NPROCESSORS_ONLN)}
# Kbuild expects a date string, not SOURCE_DATE_EPOCH's integer. This also
# controls the tiny fallback initramfs that is embedded even when the external
# appliance initramfs is supplied by VZLinuxBootLoader.
KBUILD_BUILD_TIMESTAMP=$(date -u -d "@${SOURCE_DATE_EPOCH:-0}" '+%Y-%m-%d %H:%M:%S UTC')
export KBUILD_BUILD_TIMESTAMP
export KBUILD_BUILD_USER=thunderboot
export KBUILD_BUILD_HOST=builder
export KBUILD_BUILD_VERSION=1
make -C "$source" O="$build" -j"$jobs" Image

# VZLinuxBootLoader consumes the uncompressed ARM64 boot Image, not the ELF
# vmlinux file used by Cloud Hypervisor's x86 test harness.
install -m 0644 "$build/arch/arm64/boot/Image" "$out"
install -m 0644 "$build/.config" "$(dirname "$out")/kernel.config"

description=$(file -b "$out")
case "$description" in
*Linux\ kernel\ ARM64\ boot\ executable\ Image*) ;;
*) echo "unexpected ARM64 kernel image: $description" >&2; exit 1 ;;
esac

echo "built $out (Linux $kernel_version, ARM64 Image)"
