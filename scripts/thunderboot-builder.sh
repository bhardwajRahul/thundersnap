#!/bin/sh
set -eu

instance=${THUNDERBOOT_LIMA_INSTANCE:-thunderboot-arm64}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/.." && pwd)
template="$repo/build/thunderboot/lima.yaml"

find_limactl() {
	if [ -n "${LIMACTL:-}" ]; then
		printf '%s\n' "$LIMACTL"
		return
	fi
	if command -v limactl >/dev/null 2>&1; then
		command -v limactl
		return
	fi
	for candidate in "$HOME"/.local/lima-*/bin/limactl /opt/homebrew/bin/limactl /usr/local/bin/limactl; do
		if [ -x "$candidate" ]; then
			printf '%s\n' "$candidate"
			return
		fi
	done
	echo "limactl was not found; install Lima 2.0 or newer, or set LIMACTL" >&2
	exit 1
}

limactl=$(find_limactl)

usage() {
	cat <<EOF
usage: $0 start|shell|build|verify|stop|delete|status

Environment:
  LIMACTL                       path to limactl (auto-detected when omitted)
  THUNDERBOOT_LIMA_INSTANCE     instance name (default: thunderboot-arm64)

The pinned ARM64 Debian VM mounts this repository at /work/thundersnap.
EOF
}

has_instance() {
	"$limactl" list --format '{{.Name}}' 2>/dev/null | grep -Fx "$instance" >/dev/null
}

start() {
	if ! has_instance; then
		"$limactl" start --yes --name "$instance" --param "repo=$repo" "$template"
	else
		status=$("$limactl" list "$instance" --format '{{.Status}}')
		if [ "$status" != Running ]; then
			"$limactl" start "$instance"
		fi
	fi
}

command=${1:-}
case "$command" in
start)
	start
	;;
shell)
	start
	exec "$limactl" shell --workdir /work/thundersnap "$instance"
	;;
build)
	start
	exec "$limactl" shell --workdir /work/thundersnap "$instance" -- ./scripts/build-thunderboot-appliance.sh
	;;
verify)
	start
	exec "$limactl" shell --workdir /work/thundersnap "$instance" -- ./scripts/verify-thunderboot-appliance.sh
	;;
stop)
	exec "$limactl" stop "$instance"
	;;
delete)
	if has_instance; then
		"$limactl" stop "$instance" >/dev/null 2>&1 || true
		exec "$limactl" delete --force "$instance"
	fi
	;;
status)
	exec "$limactl" list "$instance"
	;;
*)
	usage >&2
	exit 2
	;;
esac
