package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/thundersnap/thunderboot"
)

func setupStorage() (string, error) {
	params, err := kernelParams()
	if err != nil {
		return "", err
	}
	cache, err := thunderboot.ParseDiskSpec(params["thunderboot.cache"], false)
	if err != nil {
		return "", fmt.Errorf("cache spec: %w", err)
	}
	disk, err := thunderboot.ParseDiskSpec(params["thunderboot.disk"], true)
	if err != nil {
		return "", fmt.Errorf("disk spec: %w", err)
	}
	if disk.String() == "" {
		return "", fmt.Errorf("thunderboot.disk is required")
	}
	backing, err := realizeSpec(disk, "disk", "/dev/md/thunderboot-disk")
	if err != nil {
		return "", err
	}
	if cache.String() == "" {
		if err := ensureBtrfs(backing); err != nil {
			return "", err
		}
		return backing, nil
	}
	front, err := realizeSpec(cache, "cache", "/dev/md/thunderboot-cache")
	if err != nil {
		return "", err
	}
	if err := ensureBcache(front, backing); err != nil {
		return "", err
	}
	if err := ensureBtrfs("/dev/bcache0"); err != nil {
		return "", err
	}
	return "/dev/bcache0", nil
}

func kernelParams() (map[string]string, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, field := range strings.Fields(string(data)) {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			out[key] = value
		}
	}
	return out, nil
}

func realizeSpec(spec thunderboot.DiskSpec, name, mdPath string) (string, error) {
	if spec.NBD != nil {
		return connectNBD(spec.NBD)
	}
	for _, dev := range spec.Devices {
		if err := waitDevice(dev, 10*time.Second); err != nil {
			return "", err
		}
	}
	if spec.RAID == "" {
		if len(spec.Devices) != 1 {
			return "", fmt.Errorf("%s needs one device or a RAID declaration", name)
		}
		return spec.Devices[0], nil
	}
	if err := os.MkdirAll(filepath.Dir(mdPath), 0755); err != nil {
		return "", err
	}
	// Assemble an existing array first; only create a fresh one when assembly
	// fails AND every member is verified blank. Branching purely on "all members
	// blank" (as before) is dangerous: a mid-init crash can leave md superblocks
	// on only some members, so blkid reads the others as blank and a re-run
	// would `mdadm --create` over devices that may already hold data, wiping
	// it. Assemble-first never destroys data; if assembly fails and members
	// are not all blank we error out rather than risk an overwrite.
	assemble := append([]string{"--assemble", "--run", mdPath}, spec.Devices...)
	if err := run("mdadm", assemble...); err != nil {
		if !allBlank(spec.Devices) {
			return "", fmt.Errorf("md %s assemble failed and members are not all blank (refusing to create/overwrite): %w", name, err)
		}
		level := strings.TrimPrefix(spec.RAID, "raid")
		args := []string{"--create", mdPath, "--run", "--metadata=1.2", "--level=" + level,
			"--raid-devices=" + strconv.Itoa(len(spec.Devices))}
		args = append(args, spec.Devices...)
		if err := run("mdadm", args...); err != nil {
			return "", err
		}
	}
	if err := waitDevice(mdPath, 10*time.Second); err != nil {
		return "", err
	}
	return mdPath, nil
}

func connectNBD(u *url.URL) (string, error) {
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		if strings.Contains(u.Host, ":") {
			return "", fmt.Errorf("NBD URL host: %w", err)
		}
		host, port = u.Host, "10809"
	}
	export := strings.TrimPrefix(u.Path, "/")
	// -persist keeps /dev/nbd0 alive across server disconnects and retries; the
	// non-appliance NBDInitScript uses it too. -nofork keeps nbd-client in the
	// foreground so we can reap it.
	args := []string{"-nonetlink", "-nofork", "-persist"}
	if export != "" {
		args = append(args, "-N", export)
	}
	args = append(args, host, port, "/dev/nbd0")
	cmd := exec.Command("/bin/nbd-client", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// Reap nbd-client when it eventually exits so it does not become a zombie
	// child of PID 1 / the post-switch_root appliance process.
	go func() { _ = cmd.Wait() }()
	if err := waitDevice("/dev/nbd0", 10*time.Second); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for size("/dev/nbd0") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if size("/dev/nbd0") == 0 {
		return "", fmt.Errorf("timeout waiting for /dev/nbd0 capacity")
	}
	return "/dev/nbd0", nil
}

func ensureBcache(cache, backing string) error {
	cacheType := signature(cache)
	backingType := signature(backing)
	// Refuse to overwrite a conflicting (non-bcache) signature on either side.
	// A blank side is formatted; a bcache side is left alone. This makes each
	// step idempotent so a crash between `make-bcache -C` and `-B` no longer
	// leaves the appliance in an unrecoverable "partial/unknown" state: on
	// reboot the already-formatted side is kept and only the missing side is
	// formatted.
	if cacheType != "" && cacheType != "bcache" {
		return fmt.Errorf("cache %s has conflicting signature %q", cache, cacheType)
	}
	if backingType != "" && backingType != "bcache" {
		return fmt.Errorf("backing %s has conflicting signature %q", backing, backingType)
	}
	if cacheType == "" {
		if err := run("make-bcache", "-C", cache); err != nil {
			return err
		}
	}
	if backingType == "" {
		if cs, bs := size(cache), size(backing); cs > 0 && bs > 0 && cs > bs {
			return fmt.Errorf("bcache backing %s is smaller than cache %s", backing, cache)
		}
		if err := run("make-bcache", "-B", backing); err != nil {
			return err
		}
	}
	// Registration is idempotent enough for boot: EINVAL commonly means the
	// kernel auto-registered the device before init reached this point.
	for _, dev := range []string{backing, cache} {
		_ = os.WriteFile("/sys/fs/bcache/register", []byte(dev), 0200)
	}
	return waitDevice("/dev/bcache0", 10*time.Second)
}

func ensureBtrfs(device string) error {
	typeName := signature(device)
	switch typeName {
	case "":
		return run("mkfs.btrfs", "-f", "-L", "thundersnap-data", device)
	case "btrfs":
		return nil
	default:
		return fmt.Errorf("refusing to overwrite %s signature %q", device, typeName)
	}
}

func signature(device string) string {
	out, _ := exec.Command("/bin/blkid", "-p", "-o", "value", "-s", "TYPE", device).Output()
	return strings.TrimSpace(string(out))
}

func allBlank(devices []string) bool {
	for _, d := range devices {
		if signature(d) != "" {
			return false
		}
	}
	return true
}

func size(device string) int64 {
	// /dev/md/<name> and other alias symlinks are not directly under
	// /sys/class/block; resolve the symlink (e.g. /dev/md/thunderboot-disk ->
	// /dev/md127) before taking the basename, otherwise size() silently
	// returns 0 and the bcache size sanity check becomes meaningless.
	if resolved, err := filepath.EvalSymlinks(device); err == nil && resolved != "" {
		device = resolved
	}
	name := filepath.Base(device)
	data, _ := os.ReadFile(filepath.Join("/sys/class/block", name, "size"))
	sectors, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return sectors * 512
}

func waitDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

func run(name string, args ...string) error {
	cmd := exec.Command("/bin/"+name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}
