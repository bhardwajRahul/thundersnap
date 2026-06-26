// refs_resolve.go bridges the SSH/control-socket layer (which addresses frames
// by a per-user ref name) and the UUID-based frame/ref stores.
//
// Frames are isolated per tailscale user at fs/<user>/<uuid>. A ref is a
// mutable name->UUID pointer, namespaced per user so two users may both have a
// ref named "deb" without colliding. Refs are stored flat in the ref store
// using a per-user key (see refKeyForUser).
package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/refs"
)

// safeTailscaleUser extracts the sanitized tailscale user that owns this
// control socket from its rootFS path (fs/<user>/<frame>). The user is the
// first path component relative to --fs-dir.
func (c *controlServer) safeTailscaleUser() (string, error) {
	rel, err := filepath.Rel(*flagFsDir, c.rootFS)
	if err != nil {
		return "", fmt.Errorf("cannot determine tailscale user from rootFS path: %w", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 || parts[0] == "" || parts[0] == ".." {
		return "", fmt.Errorf("cannot determine tailscale user from rootFS path %q", c.rootFS)
	}
	return parts[0], nil
}

// refKeyForUser builds the flat ref-store key for a per-user ref. The ref store
// is a single flat namespace whose names must match refs.ValidateName (no '/'),
// so we encode the (already path-sanitized) user into a ref-safe prefix and
// join it to the ref name with a '.' separator.
//
//	refKeyForUser("alice@example.com", "deb") => "alice_example_com.deb"
func refKeyForUser(safeUser, name string) string {
	return refSafe(safeUser) + "." + name
}

// refSafe maps an arbitrary (path-sanitized) string to one that satisfies
// refs.ValidateName: it must start with an alphanumeric and contain only
// [a-zA-Z0-9._-]. Any other character is replaced with '_'. A leading
// non-alphanumeric is prefixed with 'u'.
func refSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			// Includes '.', '@', and anything else: collapse to '_' so we
			// never produce consecutive dots that ValidateName rejects.
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "u"
	}
	if c := out[0]; !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
		out = "u" + out
	}
	return out
}

// resolveFrameRootFS resolves a per-user frame name to the on-disk rootFS path
// of the frame it refers to: fs/<user>/<uuid>.
//
// If a ref with that name exists for the user, it resolves to the frame UUID
// the ref points at and returns the existing frame's path (it does NOT create a
// new fs/<user>/<name> directory). If the ref does not exist, an empty frame
// and ref are auto-created (temporary behavior) and the new frame's path is
// returned.
//
// A blank frameName (direct shell with no frame) is left to the caller; this
// function requires a non-empty name.
func resolveFrameRootFS(tailscaleUser, frameName string) (string, error) {
	if frameName == "" {
		return "", fmt.Errorf("resolveFrameRootFS: empty frame name")
	}
	safeUser := sanitizeForPath(tailscaleUser)
	key := refKeyForUser(safeUser, frameName)

	ref, err := refStore.Get(key)
	if err == nil {
		// Existing ref: reuse the frame it points at.
		return filepath.Join(*flagFsDir, safeUser, ref.UUID.String()), nil
	}
	if err != refs.ErrRefNotFound {
		return "", fmt.Errorf("resolveFrameRootFS: get ref %q: %w", key, err)
	}

	// Ref does not exist: auto-create an empty frame + ref.
	uuid, err := frameid.New()
	if err != nil {
		return "", fmt.Errorf("resolveFrameRootFS: generate uuid: %w", err)
	}
	rootFS := filepath.Join(*flagFsDir, safeUser, uuid.String())

	// Create an empty (blank) frame filesystem.
	if err := createFrame(rootFS, "nil::", "", "", "container"); err != nil {
		return "", fmt.Errorf("resolveFrameRootFS: create empty frame: %w", err)
	}

	// Record frame metadata (best-effort; UUIDs are globally unique so a flat
	// frame store is fine).
	if frameStore != nil {
		if err := frameStore.Create(uuid, &frames.Frame{Isolation: "container"}); err != nil {
			// Non-fatal: the on-disk frame exists; metadata is advisory.
			// Fall through and still create the ref.
		}
	}

	if err := refStore.Create(key, uuid); err != nil {
		return "", fmt.Errorf("resolveFrameRootFS: create ref %q: %w", key, err)
	}

	return rootFS, nil
}
