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

# Build the ARM64 appliance initramfs:
./scripts/thunderboot-builder.sh build

# Stop it without losing the builder disk:
./scripts/thunderboot-builder.sh stop

# Destroy it; the next start recreates it from the checked-in recipe:
./scripts/thunderboot-builder.sh delete
```

Override the default instance name with `THUNDERBOOT_LIMA_INSTANCE`. The Lima
VM is disposable build infrastructure: everything required to recreate it is
tracked here, while generated outputs remain ignored.

The initial `build` command produces `thunderboot-out/initramfs.cpio` without
nested Cloud Hypervisor payloads. The next appliance work will add the ARM64
kernel recipe and emit a versioned manifest/archive for Aperture+. Keeping this
first builder change separate makes the native ARM64 host available before
changing the guest kernel and format.

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
- builds `vmlinux` from the tracked `vm/kernel.config` (using GCC 14 when
  available because Linux 6.12 predates GCC 15's C23 default);
- stamps the kernel with the config hash so config changes force a rebuild.

`make e2e-tb` invokes this automatically. The ordinary VM-backed e2e suites
also use these files, so run the helper explicitly before those suites on a
fresh Linux checkout. Generated VM artifacts are ignored and must not be
committed.
