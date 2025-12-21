#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Check for KVM access
if [ ! -w /dev/kvm ]; then
    echo "Error: /dev/kvm not writable. You may need to add yourself to the kvm group:"
    echo "  sudo usermod -aG kvm $USER"
    echo "Then log out and back in."
    exit 1
fi

SOCKET_PATH="/tmp/virtiofs-root.sock"
rm -f "$SOCKET_PATH"

# Start virtiofsd in background
/usr/libexec/virtiofsd \
    --socket-path="$SOCKET_PATH" \
    --shared-dir=/snapshots/1 \
    --cache=always &
VIRTIOFSD_PID=$!

# Wait for socket to be created
while [ ! -S "$SOCKET_PATH" ]; do
    sleep 0.1
done

# Cleanup on exit
trap "kill $VIRTIOFSD_PID 2>/dev/null; rm -f $SOCKET_PATH /tmp/vm.vsock" EXIT

VSOCK_PATH="/tmp/vm.vsock"
rm -f "$VSOCK_PATH"

exec ./cloud-hypervisor \
    --kernel vmlinux \
    --cpus boot=1 \
    --memory size=512M,shared=on \
    --fs tag=rootfs,socket="$SOCKET_PATH" \
    --vsock cid=3,socket="$VSOCK_PATH" \
    --cmdline "console=ttyS0 rootfstype=virtiofs root=rootfs rw init=/sbin/init quiet loglevel=3" \
    --serial tty \
    --console off
