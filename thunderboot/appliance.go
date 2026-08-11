package thunderboot

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if cfg.DataDiskSize == "" {
		cfg.DataDiskSize = "2T"
	}
	for _, path := range []string{cfg.Initramfs, filepath.Join(cfg.VMDir, "cloud-hypervisor"), filepath.Join(cfg.VMDir, "vmlinux")} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("required appliance artifact %s: %w", path, err)
		}
	}
	if cfg.DataDisk == "" {
		return nil, fmt.Errorf("appliance DataDisk is required")
	}
	if cfg.ConfigDir == "" {
		return nil, fmt.Errorf("appliance ConfigDir is required")
	}
	if err := ensureBtrfsDisk(cfg.DataDisk, cfg.DataDiskSize); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}

	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", "tb-config-"+sessionID+".sock")
	passtSock := filepath.Join("/tmp", "tb-passt-"+sessionID+".sock")

	virtiofsdCmd := exec.Command("/usr/libexec/virtiofsd",
		"--socket-path="+virtiofsSock,
		"--shared-dir="+cfg.ConfigDir,
		"--cache=never",
		"--modcaps=-setfcap",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}
	cleanup := func() {
		_ = virtiofsdCmd.Process.Kill()
		_ = virtiofsdCmd.Wait()
		_ = os.Remove(virtiofsSock)
	}
	if err := waitForSocket(virtiofsSock, 5*time.Second); err != nil {
		cleanup()
		return nil, fmt.Errorf("virtiofsd socket: %w", err)
	}

	passtCmd := exec.Command("passt", "--socket", passtSock, "--vhost-user", "--foreground", "--quiet", "-a", "10.0.2.15", "-g", "10.0.2.2", "-D", "10.0.2.3")
	passtCmd.Stderr = os.Stderr
	if err := passtCmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start passt: %w", err)
	}
	cleanupAll := func() {
		_ = passtCmd.Process.Kill()
		_ = passtCmd.Wait()
		_ = os.Remove(passtSock)
		cleanup()
	}
	if err := waitForSocket(passtSock, 5*time.Second); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("passt socket: %w", err)
	}

	cmdline := "console=ttyS0 panic=-1 reboot=t ip=10.0.2.15::10.0.2.2:255.255.255.0:thunderboot:eth0:off"
	chvCmd := exec.Command(filepath.Join(cfg.VMDir, "cloud-hypervisor"),
		"--kernel", filepath.Join(cfg.VMDir, "vmlinux"),
		"--initramfs", cfg.Initramfs,
		"--cpus", "boot="+strconv.Itoa(cfg.CPUs),
		"--memory", fmt.Sprintf("size=%dM,shared=on", cfg.MemoryMB),
		"--disk", "path="+cfg.DataDisk,
		"--fs", "tag=bootconfig,socket="+virtiofsSock,
		"--net", "vhost_user=true,socket="+passtSock+",num_queues=2",
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
	)
	session := &VMSession{
		virtiofsdCmd:  virtiofsdCmd,
		passtCmd:      passtCmd,
		chvCmd:        chvCmd,
		virtiofsSock:  virtiofsSock,
		preserveDisks: true,
		done:          make(chan struct{}),
	}
	ptmx, err := pty.Start(chvCmd)
	if err != nil {
		cleanupAll()
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

func ensureBtrfsDisk(path, size string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat data disk: %w", err)
	}
	bytes, err := parseDiskSize(size)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create data disk directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("create data disk: %w", err)
	}
	if err := f.Truncate(bytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("size data disk: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	cmd := exec.Command("mkfs.btrfs", "-f", "-L", "thundersnap-data", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mkfs.btrfs: %w\n%s", err, out)
	}
	return nil
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
