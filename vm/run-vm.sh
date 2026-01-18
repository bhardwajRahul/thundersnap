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
PASST_SOCKET="/tmp/passt-vm.sock"
rm -f "$SOCKET_PATH" "$PASST_SOCKET"

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

# Start passt for user-space networking (provides outgoing network without iptables)
# Use --vhost-user mode for cloud-hypervisor's virtio-net socket interface
passt --socket "$PASST_SOCKET" --vhost-user --foreground --quiet &
PASST_PID=$!

# Wait for passt socket to be created
while [ ! -S "$PASST_SOCKET" ]; do
    sleep 0.1
done

# Cleanup on exit
trap "kill $VIRTIOFSD_PID $PASST_PID 2>/dev/null; rm -f $SOCKET_PATH $PASST_SOCKET /tmp/vm.vsock" EXIT

VSOCK_PATH="/tmp/vm.vsock"
rm -f "$VSOCK_PATH"

exec ./cloud-hypervisor \
    --kernel vmlinux \
    --cpus boot=1 \
    --memory size=512M,shared=on \
    --fs tag=rootfs,socket="$SOCKET_PATH" \
    --net vhost_user=true,socket="$PASST_SOCKET",num_queues=2 \
    --vsock cid=3,socket="$VSOCK_PATH" \
    --cmdline "console=ttyS0 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c 'mount -t proc proc /proc; ip link set eth0 up; ip addr add 10.0.2.15/24 dev eth0; ip route add default via 10.0.2.2; echo nameserver 10.0.2.3 > /etc/resolv.conf; exec /sbin/init'" \
    --serial tty \
    --console off
