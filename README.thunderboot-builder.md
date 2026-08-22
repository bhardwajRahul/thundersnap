# Thunderboot build environments

Thunderboot has two different artifact sets:

1. The **Aperture appliance** is an ARM64 Linux kernel and initramfs booted
   directly by macOS Virtualization.framework. It is built in the pinned Lima
   environment described below.
2. The existing Linux/KVM development and `e2e-tb` tests use an x86-64 Linux
   kernel and Cloud Hypervisor. Those generated artifacts are fetched/built on
   demand and are no longer committed to git.

## ARM64 Aperture appliance builder

The canonical builder is `build/thunderboot/lima.yaml`. It pins a Debian 13
ARM64 cloud image by immutable URL and SHA-512, pins apt to a dated Debian
snapshot, installs the native ARM64 kernel/appliance toolchain, and installs
the Go version declared by this repository. The repository is mounted
read/write at `/work/thundersnap`, so outputs appear on the host in
`thunderboot-out/`.

Install Lima 2.0 or newer on an Apple-silicon Mac. The helper finds `limactl`
on `PATH`, in Homebrew's usual locations, or beneath
`~/.local/lima-*/bin`. Set `LIMACTL=/path/to/limactl` to override discovery.

```sh
# Create/provision (first run) or start the pinned builder:
./scripts/thunderboot-builder.sh start

# Enter it at /work/thundersnap:
./scripts/thunderboot-builder.sh shell

# Build and package the ARM64 kernel and appliance initramfs:
./scripts/thunderboot-builder.sh build

# Re-verify an existing archive without rebuilding it:
./scripts/thunderboot-builder.sh verify

# Stop it without losing the builder disk:
./scripts/thunderboot-builder.sh stop

# Destroy it; the next start recreates it from the checked-in recipe:
./scripts/thunderboot-builder.sh delete
```

Override the default instance name with `THUNDERBOOT_LIMA_INSTANCE`. The Lima
VM is disposable build infrastructure: everything required to recreate it is
tracked here, while generated outputs remain ignored.

The `build` command produces:

```text
thunderboot-out/
  Image
  initramfs.cpio
  kernel.config
  manifest.json
  thunderboot-appliance-linux-arm64.tar.zst
```

`Image` is the uncompressed ARM64 boot image consumed by Apple's
`VZLinuxBootLoader`. The initramfs deliberately omits nested Cloud Hypervisor
payloads. `manifest.json` records the source revision, kernel version, sizes,
and SHA-256 hashes; the archive is the complete import unit for Aperture+.
Build timestamps and archive metadata derive from the source commit so two
clean builds of the same revision have stable outputs. Packaging and the
standalone `verify` command reject incorrect hashes, unexpected archive
members, mixed-architecture ELF files, missing tools, and nested-VM payloads.

The tracked ARM64 kernel configuration is a small hardware/VMM fragment
layered onto the tracked `vm/kernel.config`. The latter is the canonical
x86-64 Cloud Hypervisor guest configuration and supplies the common
filesystem, bcache, networking, namespace, cgroup, and security features; the
ARM64 build feeds those settings through ARM64 Kconfig and then adds the Apple
Virtualization.framework requirements. This keeps the amd64 appliance and
Cloud Hypervisor guest on the same config instead of maintaining two copies.
The build checks every requested ARM64 setting after Kconfig resolves
dependencies. Kernel source is Linux 6.12.8, downloaded and verified by
SHA-256 into the Lima VM's disposable build cache.

## Linux/KVM test artifacts

`vm/cloud-hypervisor` and `vm/vmlinux` used to be opaque checked-in x86-64
binaries. They are removed from the current tree, although their objects remain
in existing git history unless that history is rewritten separately. Generate
them on an x86-64 Debian/Linux test host instead:

```sh
./scripts/fetch-thunderboot-vm-artifacts.sh
```

The script:

- downloads Cloud Hypervisor v49.0's static x86-64 release and verifies its
  SHA-256;
- downloads Linux 6.12.8 and verifies its SHA-256;
- builds `vmlinux` from the tracked `vm/kernel.config` (the shared amd64
  appliance/Cloud Hypervisor configuration, using GCC 14 when available
  because Linux 6.12 predates GCC 15's C23 default);
- stamps the kernel with the config hash so config changes force a rebuild.

`make e2e-tb` invokes this automatically. The ordinary VM-backed e2e suites
also use these files, so run the helper explicitly before those suites on a
fresh Linux checkout. Generated VM artifacts are ignored and must not be
committed.
