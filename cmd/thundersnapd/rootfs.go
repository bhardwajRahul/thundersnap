package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/tsm"
)

// prepareContainerRootFS sets up a container's root filesystem for use.
// It ensures the rootFS exists (cloning from baseUserFS or a base snapshot),
// creates required mount points (/proc), and copies the ts binary.
// This is the common setup needed before running any container session.
func prepareContainerRootFS(rootFS, baseUserFS string) error {
	if err := ensureRootFS(rootFS, baseUserFS); err != nil {
		return fmt.Errorf("set up root filesystem: %w", err)
	}

	// Ensure /proc mount point exists in the rootfs
	procDir := filepath.Join(rootFS, "proc")
	if err := os.MkdirAll(procDir, 0555); err != nil {
		return fmt.Errorf("create /proc directory: %w", err)
	}

	// Copy ts binary into container's /bin using btrfs reflink
	if err := copyTsBinary(rootFS); err != nil {
		return fmt.Errorf("copy ts binary: %w", err)
	}

	return nil
}

// ensureRootFS ensures the root filesystem exists at the given path.
// If it doesn't exist, it first creates an intermediate snapshot in snaps-dir,
// then clones from that to the destination. This ensures snaps-dir contains
// stable reference points while fs-dir contains the live, changing filesystems.
//
// If a frame.jsonc file exists at rootFS+".jsonc", the frame model is used:
// - The rootfs, home, and work snaps are cloned to create a three-component frame
// - Nested /home and /work subvolumes are created within the rootfs
// - Taints are computed as the union of all component snaps' taints
//
// The snapshotting flow (legacy single-component):
// 1. Determine source: baseUserFS (if exists) or $snaps-dir/1
// 2. Create intermediate snapshot in $snaps-dir with random hex ID
// 3. Clone from intermediate snapshot to rootFS in $fs-dir
// 4. Create .stamp files tracking the base snapshot ID
func ensureRootFS(rootFS, baseUserFS string) error {
	// Check if the directory already exists
	if _, err := os.Stat(rootFS); err == nil {
		return nil // Already exists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking rootfs: %w", err)
	}

	// Check if a frame sidecar exists specifying the frame composition
	frameMeta, err := readFrameSidecar(rootFS)
	if err != nil {
		return fmt.Errorf("reading frame meta: %w", err)
	}

	if frameMeta != nil {
		// Use the three-component frame model (may be nil:nil:nil for empty frame)
		return ensureFrameFS(rootFS, frameMeta)
	}

	// No sidecar exists: treat as an empty frame (nil:nil:nil).
	// This is the default for fresh unattached frames (e.g., first SSH login).
	return ensureFrameFS(rootFS, &frames.Frame{Isolation: "container"})
}

// ensureFrameFS creates a three-component frame from the given frame metadata.
// It creates:
// - rootFS: the rootfs subvolume (the frame directory itself)
// - rootFS/home: nested home subvolume
// - rootFS/work: nested work subvolume
//
// If meta.Rootfs is empty (nil:nil:nil frame spec), creates an empty rootfs
// with minimal directory structure needed for the container to function.
func ensureFrameFS(rootFS string, meta *frames.Frame) error {
	// Ensure the parent directory exists
	parentDir := filepath.Dir(rootFS)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Clone rootfs component from snapshot, or create empty rootfs
	if meta.Rootfs != "" {
		// Clone from existing snapshot
		rootfsSnapPath := filepath.Join(*flagSnapsDir, meta.Rootfs)
		if _, err := os.Stat(rootfsSnapPath); err != nil {
			return fmt.Errorf("rootfs snap %s: %w", meta.Rootfs, err)
		}

		if err := btrfsSnapshot(rootfsSnapPath, rootFS, false); err != nil {
			return err
		}
	} else {
		// Create empty rootfs subvolume with minimal structure
		if err := btrfsCreateSubvol(rootFS); err != nil {
			return err
		}

		// Set up minimal directory structure for a functional container
		if err := setupMinimalRootfs(rootFS); err != nil {
			return fmt.Errorf("setup minimal rootfs: %w", err)
		}
	}

	// Step 2: Create or clone home subvolume
	homePath := filepath.Join(rootFS, "home")
	// Remove existing /home directory if it's not a subvolume (from the rootfs snap)
	if fi, err := os.Stat(homePath); err == nil && fi.IsDir() && !isSubvolume(homePath) {
		if err := os.RemoveAll(homePath); err != nil {
			log.Printf("Warning: failed to remove existing /home directory: %v", err)
		}
	}

	if meta.Home != "" {
		// Clone from home snap
		homeSnapPath := filepath.Join(*flagSnapsDir, meta.Home)
		if _, err := os.Stat(homeSnapPath); err != nil {
			return fmt.Errorf("home snap %s: %w", meta.Home, err)
		}
		if err := btrfsSnapshot(homeSnapPath, homePath, false); err != nil {
			return err
		}
	} else {
		// Create empty home subvolume
		if err := btrfsCreateSubvol(homePath); err != nil {
			return err
		}
		// Chown to the thundersnap user (UID 7575)
		if err := os.Chown(homePath, tsm.ThundersnapUID, tsm.ThundersnapGID); err != nil {
			log.Printf("Warning: failed to chown home subvolume: %v", err)
		}
	}

	// Step 3: Create or clone work subvolume
	workPath := filepath.Join(rootFS, "work")
	// Remove existing /work directory if it's not a subvolume
	if fi, err := os.Stat(workPath); err == nil && fi.IsDir() && !isSubvolume(workPath) {
		if err := os.RemoveAll(workPath); err != nil {
			log.Printf("Warning: failed to remove existing /work directory: %v", err)
		}
	}

	if meta.Work != "" {
		// Clone from work snap
		workSnapPath := filepath.Join(*flagSnapsDir, meta.Work)
		if _, err := os.Stat(workSnapPath); err != nil {
			return fmt.Errorf("work snap %s: %w", meta.Work, err)
		}
		if err := btrfsSnapshot(workSnapPath, workPath, false); err != nil {
			return err
		}
	} else {
		// Create empty work subvolume
		if err := btrfsCreateSubvol(workPath); err != nil {
			return err
		}
		// Chown to the thundersnap user (UID 7575)
		if err := os.Chown(workPath, tsm.ThundersnapUID, tsm.ThundersnapGID); err != nil {
			log.Printf("Warning: failed to chown work subvolume: %v", err)
		}
	}

	// Step 3b: Ensure /home/work is a convenient symlink to /work. If nothing
	// named "work" already exists in the home subvolume, create the symlink so
	// users landing in /home can reach the work tree at ~/work.
	homeWorkPath := filepath.Join(homePath, "work")
	if _, err := os.Lstat(homeWorkPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink("/work", homeWorkPath); err != nil {
			log.Printf("Warning: failed to create /home/work symlink: %v", err)
		}
	}

	// Step 4: Compute taints as union of all component snaps' taints
	rootfsTaints := getSnapTaints(*flagSnapsDir, meta.Rootfs)
	homeTaints := getSnapTaints(*flagSnapsDir, meta.Home)
	workTaints := getSnapTaints(*flagSnapsDir, meta.Work)
	meta.Taints = UnionTaints(rootfsTaints, homeTaints, workTaints)

	// Step 5: Write frame.jsonc with updated taints
	if err := writeFrameSidecar(rootFS, meta); err != nil {
		log.Printf("Warning: failed to write frame.jsonc for %s: %v", rootFS, err)
	}

	// Step 6: Write stamp file (rootfs snap ID for compatibility)
	if err := writeStampFile(rootFS, meta.Rootfs); err != nil {
		log.Printf("Warning: failed to write stamp file for %s: %v", rootFS, err)
	}

	// Step 7-9: Ensure the "user" account, sudoers, resolv.conf, and /tmp.
	finalizeFrameRootfs(rootFS)

	// Step 10: Create /id subvolume for frame-local secrets (never persisted in snapshots)
	// This is always created fresh and empty, never cloned from a snapshot.
	// Since it's a btrfs subvolume, it's automatically excluded from snapshots.
	idPath := filepath.Join(rootFS, "id")
	// Remove existing /id directory if it's not a subvolume (from the rootfs snap)
	if fi, err := os.Stat(idPath); err == nil && fi.IsDir() && !isSubvolume(idPath) {
		if err := os.RemoveAll(idPath); err != nil {
			log.Printf("Warning: failed to remove existing /id directory: %v", err)
		}
	}
	if !isSubvolume(idPath) {
		if err := btrfsCreateSubvol(idPath); err != nil {
			return err
		}
		// Set permissions: 0700 (only root can access)
		if err := os.Chmod(idPath, 0700); err != nil {
			log.Printf("Warning: failed to chmod /id subvolume: %v", err)
		}
	}

	log.Printf("Created frame %s with rootfs:%s home:%s work:%s taints:%v",
		rootFS, meta.Rootfs, meta.Home, meta.Work, meta.Taints)
	return nil
}

// finalizeFrameRootfs performs the common post-clone setup for a frame's root
// filesystem: ensure the "user" account (UID/GID 7575) exists in /etc/passwd,
// ensure sudoers, copy in the host's resolv.conf for DNS, and fix /tmp
// permissions. Each step is best-effort and logs a warning on failure rather
// than aborting frame setup.
func finalizeFrameRootfs(rootFS string) {
	if _, err := tsm.EnsureUserInPasswd(rootFS); err != nil {
		log.Printf("Warning: EnsureUserInPasswd on %s: %v", rootFS, err)
	}
	if err := tsm.EnsureSudoers(rootFS); err != nil {
		log.Printf("Warning: EnsureSudoers on %s: %v", rootFS, err)
	}
	if err := ensureResolvConf(rootFS); err != nil {
		log.Printf("Warning: ensure resolv.conf on %s: %v", rootFS, err)
	}
	if err := ensureTmpDir(rootFS); err != nil {
		log.Printf("Warning: ensure /tmp on %s: %v", rootFS, err)
	}
}

// ensureResolvConf copies the host's /etc/resolv.conf into the frame if
// the frame doesn't already have one. If there's an existing resolv.conf,
// it's backed up to resolv.conf.orig (but only if .orig doesn't exist).
func ensureResolvConf(rootFS string) error {
	frameResolvConf := filepath.Join(rootFS, "etc", "resolv.conf")
	frameResolvConfOrig := frameResolvConf + ".orig"
	hostResolvConf := "/etc/resolv.conf"

	// Read the host's resolv.conf
	hostData, err := os.ReadFile(hostResolvConf)
	if err != nil {
		return fmt.Errorf("reading host resolv.conf: %w", err)
	}

	// Check if frame already has a resolv.conf
	frameData, err := os.ReadFile(frameResolvConf)
	if err == nil {
		// Frame has an existing resolv.conf - check if it matches host
		if string(frameData) == string(hostData) {
			// Already matches, nothing to do
			return nil
		}
		// Different content - back up to .orig if .orig doesn't exist
		if _, err := os.Stat(frameResolvConfOrig); os.IsNotExist(err) {
			if err := os.WriteFile(frameResolvConfOrig, frameData, 0644); err != nil {
				log.Printf("Warning: failed to backup resolv.conf to %s: %v", frameResolvConfOrig, err)
			}
		}
	}

	// Ensure /etc directory exists
	etcDir := filepath.Join(rootFS, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return fmt.Errorf("creating /etc directory: %w", err)
	}

	// Write the host's resolv.conf to the frame
	if err := os.WriteFile(frameResolvConf, hostData, 0644); err != nil {
		return fmt.Errorf("writing resolv.conf: %w", err)
	}

	return nil
}

// ensureTmpDir ensures /tmp exists with the correct permissions (1777 with sticky bit).
// Docker images sometimes have /tmp with wrong permissions, which breaks apt-get and
// other tools that need to create temp files.
func ensureTmpDir(rootFS string) error {
	tmpDir := filepath.Join(rootFS, "tmp")

	// Create /tmp if it doesn't exist
	if err := os.MkdirAll(tmpDir, 0777); err != nil {
		return fmt.Errorf("creating /tmp: %w", err)
	}

	// Set correct permissions: 1777 (sticky bit + world writable)
	// The sticky bit (01000) ensures users can only delete their own files
	if err := os.Chmod(tmpDir, 01777); err != nil {
		return fmt.Errorf("chmod /tmp: %w", err)
	}

	return nil
}

// setupMinimalRootfs creates the minimal directory structure and files needed
// for a functional container when no rootfs snapshot is provided (nil:nil:nil).
// This allows creating "blank" containers that still work with SSH and ts commands.
func setupMinimalRootfs(rootFS string) error {
	// Create essential directories
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{"bin", 0755},
		{"sbin", 0755},
		{"etc", 0755},
		{"tmp", 01777}, // sticky bit
		{"proc", 0555},
		{"sys", 0555},
		{"dev", 0755},
		{"root", 0700},
		{"var", 0755},
		{"var/log", 0755},
		{"var/tmp", 01777},
		{"run", 0755},
		{"usr", 0755},
		{"usr/bin", 0755},
		{"usr/sbin", 0755},
		{"usr/lib", 0755},
	}

	for _, d := range dirs {
		path := filepath.Join(rootFS, d.path)
		if err := os.MkdirAll(path, d.mode); err != nil {
			return fmt.Errorf("mkdir %s: %w", d.path, err)
		}
		// MkdirAll doesn't set mode correctly for existing dirs or sticky bit
		if err := os.Chmod(path, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", d.path, err)
		}
	}

	// Create minimal /etc/passwd
	passwdContent := "root:x:0:0:root:/root:/bin/sh\n" +
		fmt.Sprintf("user:x:%d:%d:user:/home/user:/bin/sh\n", tsm.ThundersnapUID, tsm.ThundersnapGID) +
		"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"
	if err := os.WriteFile(filepath.Join(rootFS, "etc/passwd"), []byte(passwdContent), 0644); err != nil {
		return fmt.Errorf("write /etc/passwd: %w", err)
	}

	// Create minimal /etc/group
	groupContent := "root:x:0:\n" +
		fmt.Sprintf("user:x:%d:\n", tsm.ThundersnapGID) +
		"nogroup:x:65534:\n"
	if err := os.WriteFile(filepath.Join(rootFS, "etc/group"), []byte(groupContent), 0644); err != nil {
		return fmt.Errorf("write /etc/group: %w", err)
	}

	// Create /etc/hostname
	if err := os.WriteFile(filepath.Join(rootFS, "etc/hostname"), []byte("thundersnap\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/hostname: %w", err)
	}

	// Create /etc/hosts
	hostsContent := "127.0.0.1\tlocalhost\n::1\tlocalhost\n"
	if err := os.WriteFile(filepath.Join(rootFS, "etc/hosts"), []byte(hostsContent), 0644); err != nil {
		return fmt.Errorf("write /etc/hosts: %w", err)
	}

	// Create /etc/resolv.conf (placeholder - will be overwritten at runtime)
	if err := os.WriteFile(filepath.Join(rootFS, "etc/resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/resolv.conf: %w", err)
	}

	// Create /etc/nsswitch.conf for basic name resolution
	nsswitchContent := "passwd: files\ngroup: files\nhosts: files dns\n"
	if err := os.WriteFile(filepath.Join(rootFS, "etc/nsswitch.conf"), []byte(nsswitchContent), 0644); err != nil {
		return fmt.Errorf("write /etc/nsswitch.conf: %w", err)
	}

	log.Printf("Created minimal rootfs at %s", rootFS)
	return nil
}

// copyTsBinary copies the ts binary into the container's /bin using btrfs reflink (COW copy).
// If the container has no /bin/sh, it also creates a symlink /bin/sh -> ts so that
// SSH commands work (ssh invokes /bin/sh -c "command"). The ts binary has a minimal
// shell mode that handles this case.
func copyTsBinary(rootFS string) error {
	// Remove legacy /sbin/ts if present (we moved to /bin/ts for PATH sanity).
	os.Remove(filepath.Join(rootFS, "sbin", "ts"))

	if err := copyBinaryToRootFS(rootFS, "ts", "bin/ts"); err != nil {
		return err
	}

	// If there's no shell, symlink /bin/sh -> ts so SSH command execution works.
	// ts has a minimal shell mode that activates when invoked as "sh".
	shPath := filepath.Join(rootFS, "bin", "sh")
	if _, err := os.Lstat(shPath); os.IsNotExist(err) {
		// No shell exists - create symlink to ts
		if err := os.Symlink("ts", shPath); err != nil {
			// Non-fatal: log but don't fail
			log.Printf("Warning: failed to create /bin/sh symlink: %v", err)
		}
	}

	return nil
}

// copyVshdBinary copies the vshd binary into the VM's /sbin using btrfs reflink (COW copy).
func copyVshdBinary(rootFS string) error {
	return copyBinaryToRootFS(rootFS, "vshd", "sbin/vshd")
}

// setupFsDirLibexec creates $fs-dir/libexec/ and copies binaries there.
// This ensures binaries are on the same btrfs filesystem as frames, allowing
// reflink copies to work even when the original libexec-dir is on a different
// filesystem (e.g., /usr/libexec/thundersnap on ext4, fs-dir on btrfs).
func setupFsDirLibexec() error {
	fsDirLibexec = filepath.Join(*flagFsDir, "libexec")
	if err := os.MkdirAll(fsDirLibexec, 0755); err != nil {
		return fmt.Errorf("create %s: %w", fsDirLibexec, err)
	}

	// List of binaries to copy
	binaries := []string{"ts", "vshd"}

	for _, name := range binaries {
		src := filepath.Join(*flagLibexecDir, name)
		dst := filepath.Join(fsDirLibexec, name)

		// Check if source exists
		srcInfo, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("%s binary not found at %s: %w", name, src, err)
		}

		// Check if destination exists and is up to date
		if dstInfo, err := os.Stat(dst); err == nil {
			// Destination exists - check if it's the same size and not older
			if dstInfo.Size() == srcInfo.Size() && !dstInfo.ModTime().Before(srcInfo.ModTime()) {
				log.Printf("libexec/%s is up to date", name)
				continue
			}
		}

		// Remove existing destination
		os.Remove(dst)

		// Try btrfs reflink first
		cmd := exec.Command("cp", "--reflink=always", src, dst)
		if _, err := cmd.CombinedOutput(); err != nil {
			// Reflink failed (cross-device), fall back to regular copy
			log.Printf("Reflink copy of %s failed (expected if cross-device), using regular copy: %v", name, err)
			cmd = exec.Command("cp", src, dst)
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to copy %s to %s: %w\noutput: %s", src, dst, err, string(output))
			}
		}

		// Make it executable
		if err := os.Chmod(dst, 0755); err != nil {
			return fmt.Errorf("chmod %s: %w", dst, err)
		}

		log.Printf("Copied %s to %s", name, dst)
	}

	return nil
}

// copyBinaryToRootFS copies a binary from the fs-dir libexec directory into the rootfs.
// It uses btrfs reflink (COW copy) which requires source and destination to be on the
// same btrfs filesystem. The source is $fs-dir/libexec/<binary>, which was populated
// by setupFsDirLibexec() at startup.
func copyBinaryToRootFS(rootFS, binaryName, destPath string) error {
	// Use the fs-dir libexec directory as source (same filesystem as rootFS)
	src := filepath.Join(fsDirLibexec, binaryName)

	// Destination in rootfs
	dst := filepath.Join(rootFS, destPath)

	// Check if source exists
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s binary not found at %s (run setupFsDirLibexec first): %w", binaryName, src, err)
	}

	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	// Remove existing destination if present (reflink won't overwrite)
	os.Remove(dst)

	// Use cp --reflink=always for btrfs COW copy
	// This should now always work since source and destination are on the same btrfs
	cmd := exec.Command("cp", "--reflink=always", src, dst)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp --reflink=always failed: %w\noutput: %s", err, string(output))
	}

	// Make it executable
	if err := os.Chmod(dst, 0755); err != nil {
		return fmt.Errorf("chmod %s binary: %w", binaryName, err)
	}

	return nil
}
