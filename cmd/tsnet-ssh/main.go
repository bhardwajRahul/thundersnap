// tsnet-ssh is a simple Tailscale tsnet-based SSH server that accepts
// connections from any user and returns the output of "ps axu".
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

func main() {
	hostname := flag.String("hostname", "tsnet-ssh", "Tailscale hostname for this server")
	stateDir := flag.String("state-dir", "", "Directory to store Tailscale state (default: ~/.config/tsnet-ssh)")
	fsDir := flag.String("fs-dir", "", "Directory to store per-user filesystems (required)")
	flag.Parse()

	if *fsDir == "" {
		log.Fatalf("-fs-dir is required")
	}

	// Set up state directory
	if *stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Failed to get home directory: %v", err)
		}
		*stateDir = filepath.Join(home, ".config", "tsnet-ssh")
	}

	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		log.Fatalf("Failed to create state directory: %v", err)
	}

	// Create tsnet server
	srv := &tsnet.Server{
		Hostname: *hostname,
		Dir:      *stateDir,
	}
	defer srv.Close()

	// Start the tsnet server and wait for it to be ready
	log.Printf("Starting tsnet server with hostname %q...", *hostname)
	status, err := srv.Up(context.Background())
	if err != nil {
		log.Fatalf("Failed to start tsnet server: %v", err)
	}
	log.Printf("tsnet server is up! Tailscale IP: %v", status.TailscaleIPs)

	// Listen on port 22 for SSH connections
	ln, err := srv.Listen("tcp", ":22")
	if err != nil {
		log.Fatalf("Failed to listen on :22: %v", err)
	}
	defer ln.Close()

	log.Printf("SSH server listening on port 22")

	// Get the LocalClient to look up peer info
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("Failed to get LocalClient: %v", err)
	}

	// Create SSH server with gliderlabs/ssh
	sshServer := &ssh.Server{
		Handler: func(s ssh.Session) {
			log.Printf("New SSH session from %s (user: %s)", s.RemoteAddr(), s.User())

			// Look up the Tailscale identity of the connecting peer
			tailscaleUser := getTailscaleUser(s.Context(), lc, s.RemoteAddr().String())

			// Print greeting to stdout (PTY merges stdout/stderr anyway)
			fmt.Fprintf(s, "* Hello <%s>, connecting you to <%s>\r\n", tailscaleUser, s.User())

			// Helper to log error to both server log and client
			logErr := func(format string, args ...any) {
				msg := fmt.Sprintf(format, args...)
				log.Print(msg)
				fmt.Fprintf(s, "* Error: %s\r\n", msg)
			}

			// Sanitize usernames for filesystem paths (replace unsafe chars)
			safeTailscaleUser := sanitizeForPath(tailscaleUser)
			safeSSHUser := sanitizeForPath(s.User())

			// Set up the root filesystem for this user
			rootFS := filepath.Join(*fsDir, safeTailscaleUser, safeSSHUser)
			if err := ensureRootFS(rootFS); err != nil {
				logErr("Failed to set up root filesystem: %v", err)
				s.Exit(1)
				return
			}

			// Create home directory inside the root filesystem
			homeDir := filepath.Join("home", safeTailscaleUser)
			homeDirFull := filepath.Join(rootFS, homeDir)
			if err := os.MkdirAll(homeDirFull, 0755); err != nil {
				logErr("Failed to create home directory: %v", err)
				s.Exit(1)
				return
			}

			// Start an interactive shell
			ptyReq, winCh, isPty := s.Pty()

			cmd := exec.Command("/bin/sh")
			cmd.Dir = "/" + homeDir
			cmd.Env = []string{
				"HOME=/" + homeDir,
				"SSH_USER=" + s.User(),
				"TAILSCALE_USER=" + tailscaleUser,
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"SHELL=/bin/sh",
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Chroot: rootFS,
			}

			if isPty {
				cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
				ptmx, err := pty.Start(cmd)
				if err != nil {
					logErr("Failed to start shell: %v", err)
					s.Exit(1)
					return
				}
				defer ptmx.Close()

				// Handle window size changes
				go func() {
					for win := range winCh {
						setWinsize(ptmx, win.Width, win.Height)
					}
				}()

				// Copy data between SSH session and PTY
				go func() {
					io.Copy(ptmx, s) // stdin
				}()
				io.Copy(s, ptmx) // stdout

				cmd.Wait()
				s.Exit(cmd.ProcessState.ExitCode())
			} else {
				// No PTY requested, run without one
				cmd.Stdin = s
				cmd.Stdout = s
				cmd.Stderr = s.Stderr()
				if err := cmd.Run(); err != nil {
					logErr("Failed to run shell: %v", err)
					s.Exit(1)
					return
				}
				s.Exit(cmd.ProcessState.ExitCode())
			}
		},
		// Accept any public key (no authentication required beyond Tailscale)
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			log.Printf("Public key auth attempt from %s (user: %s) - accepting", ctx.RemoteAddr(), ctx.User())
			return true
		},
		// Accept any password (no authentication required beyond Tailscale)
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			log.Printf("Password auth attempt from %s (user: %s) - accepting", ctx.RemoteAddr(), ctx.User())
			return true
		},
	}

	log.Printf("Waiting for SSH connections...")

	// Serve SSH connections
	if err := sshServer.Serve(ln); err != nil {
		log.Fatalf("SSH server error: %v", err)
	}
}

// getTailscaleUser looks up the Tailscale identity for the given remote address.
// Returns the user's login name, or tags if it's a tagged node, or the IP if lookup fails.
func getTailscaleUser(ctx context.Context, lc *tailscale.LocalClient, remoteAddr string) string {
	// Parse the IP from the remote address (format is "ip:port")
	host := remoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		host = remoteAddr[:idx]
	}
	// Handle IPv6 addresses wrapped in brackets
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Sprintf("unknown (bad IP: %v)", err)
	}

	// Look up who owns this IP
	whois, err := lc.WhoIs(ctx, ip.String())
	if err != nil {
		return fmt.Sprintf("unknown (whois error: %v)", err)
	}

	// If it's a tagged node, return the tags
	if whois.Node != nil && len(whois.Node.Tags) > 0 {
		return fmt.Sprintf("tags: %s", strings.Join(whois.Node.Tags, ", "))
	}

	// Return the user's login name
	if whois.UserProfile != nil && whois.UserProfile.LoginName != "" {
		return whois.UserProfile.LoginName
	}

	return "unknown"
}

// setWinsize sets the size of the given pty.
func setWinsize(f *os.File, w, h int) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0})))
}

// sanitizeForPath replaces characters that are unsafe for filesystem paths.
func sanitizeForPath(s string) string {
	// Replace / and null bytes, and collapse multiple replacements
	replacer := strings.NewReplacer(
		"/", "_",
		"\x00", "_",
		"..", "_",
	)
	result := replacer.Replace(s)
	// Also handle leading dots to prevent hidden directories
	result = strings.TrimLeft(result, ".")
	if result == "" {
		result = "_"
	}
	return result
}

// ensureRootFS ensures the root filesystem exists at the given path.
// If it doesn't exist, it clones from /snapshots/1 using btrfs.
func ensureRootFS(rootFS string) error {
	// Check if the directory already exists
	if _, err := os.Stat(rootFS); err == nil {
		return nil // Already exists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking rootfs: %w", err)
	}

	// Ensure the parent directory exists
	parentDir := filepath.Dir(rootFS)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Clone from /snapshots/1 using btrfs subvolume snapshot
	cmd := exec.Command("btrfs", "subvolume", "snapshot", "/snapshots/1", rootFS)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot failed: %w\noutput: %s", err, string(output))
	}

	return nil
}
