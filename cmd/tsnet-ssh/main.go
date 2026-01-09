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
	flag.Parse()

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

			// Print greeting to stderr
			fmt.Fprintf(s.Stderr(), "* Hello <%s>, connecting you to <%s>\r\n", tailscaleUser, s.User())

			// Start an interactive shell
			ptyReq, winCh, isPty := s.Pty()

			cmd := exec.Command("/bin/sh")
			cmd.Env = append(os.Environ(),
				"SSH_USER="+s.User(),
				"TAILSCALE_USER="+tailscaleUser,
			)

			if isPty {
				cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
				ptmx, err := pty.Start(cmd)
				if err != nil {
					fmt.Fprintf(s.Stderr(), "Error starting pty: %v\n", err)
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
