package thunderboot

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
)

func startApplianceVM(cfg VMConfig) (*VMSession, error) {
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 2048
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.ApplianceDiskSize == "" {
		cfg.ApplianceDiskSize = "2T"
	}
	for _, path := range []string{cfg.Initramfs, filepath.Join(cfg.VMDir, "cloud-hypervisor"), filepath.Join(cfg.VMDir, "vmlinux")} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("required appliance artifact %s: %w", path, err)
		}
	}
	cache, err := ParseDiskSpec(cfg.ApplianceCache, false)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	disk, err := ParseDiskSpec(cfg.ApplianceDisk, true)
	if err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}
	if len(disk.Devices) == 0 && disk.NBD == nil {
		return nil, fmt.Errorf("appliance disk is required")
	}

	// Translate each host image path into the corresponding virtio-blk device.
	var hostDisks []string
	translate := func(spec *DiskSpec) error {
		for i, path := range spec.Devices {
			if err := ensureSparseDisk(path, cfg.ApplianceDiskSize); err != nil {
				return err
			}
			hostDisks = append(hostDisks, path)
			if len(hostDisks) > 26 {
				return fmt.Errorf("too many appliance disks")
			}
			spec.Devices[i] = fmt.Sprintf("/dev/vd%c", 'a'+len(hostDisks)-1)
		}
		return nil
	}
	if err := translate(&cache); err != nil {
		return nil, err
	}
	if err := translate(&disk); err != nil {
		return nil, err
	}

	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	passtSock := filepath.Join("/tmp", "tb-passt-"+sessionID+".sock")
	passtCmd := exec.Command("passt", "--socket", passtSock, "--vhost-user", "--foreground", "--quiet",
		"-a", "10.0.2.15", "-g", "10.0.2.2", "--dns-forward", "10.0.2.3")
	passtCmd.Stderr = os.Stderr
	if err := passtCmd.Start(); err != nil {
		return nil, fmt.Errorf("start passt: %w", err)
	}
	cleanup := func() {
		_ = passtCmd.Process.Kill()
		_ = passtCmd.Wait()
		_ = os.Remove(passtSock)
	}
	if err := waitForSocket(passtSock, 5*time.Second); err != nil {
		cleanup()
		return nil, fmt.Errorf("passt socket: %w", err)
	}

	cmdline := []string{
		"console=ttyS0", "panic=-1", "reboot=t",
		"ip=10.0.2.15::10.0.2.2:255.255.255.0:thunderboot:eth0:off",
		"thunderboot.disk=" + disk.String(),
	}
	if cache.String() != "" {
		cmdline = append(cmdline, "thunderboot.cache="+cache.String())
	}
	if cfg.TestOnly {
		cmdline = append(cmdline, "thundersnap.testonly=storage")
	}
	args := []string{
		"--kernel", filepath.Join(cfg.VMDir, "vmlinux"),
		"--initramfs", cfg.Initramfs,
		"--cpus", "boot=" + strconv.Itoa(cfg.CPUs),
		"--memory", fmt.Sprintf("size=%dM,shared=on", cfg.MemoryMB),
	}
	if len(hostDisks) > 0 {
		args = append(args, "--disk")
		for _, path := range hostDisks {
			args = append(args, "path="+path)
		}
	}
	args = append(args,
		"--net", "vhost_user=true,socket="+passtSock+",num_queues=2",
		"--cmdline", strings.Join(cmdline, " "),
		"--serial", "tty", "--console", "off", "--pvpanic",
	)
	chvCmd := exec.Command(filepath.Join(cfg.VMDir, "cloud-hypervisor"), args...)
	session := &VMSession{passtCmd: passtCmd, chvCmd: chvCmd, preserveDisks: true, done: make(chan struct{})}
	ptmx, err := pty.Start(chvCmd)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	session.consolePtmx = ptmx
	session.consoleOut = bufio.NewScanner(ptmx)
	log.Printf("cloud-hypervisor appliance started with PID %d", chvCmd.Process.Pid)
	go func() {
		_ = chvCmd.Wait()
		_ = ptmx.Close()
		log.Printf("cloud-hypervisor appliance exited")
		close(session.done)
	}()
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			log.Printf("vm: %s", scanner.Text())
		}
	}()
	return session, nil
}

func ensureSparseDisk(path, size string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat disk: %w", err)
	}
	bytes, err := parseDiskSize(size)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := f.Truncate(bytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func parseDiskSize(size string) (int64, error) {
	if len(size) < 2 {
		return 0, fmt.Errorf("invalid disk size %q", size)
	}
	units := map[byte]int64{'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	multiplier, ok := units[size[len(size)-1]]
	if !ok {
		return 0, fmt.Errorf("disk size must end in M, G, or T")
	}
	n, err := strconv.ParseInt(size[:len(size)-1], 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid disk size %q", size)
	}
	return n * multiplier, nil
}
