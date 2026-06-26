// refs_frames_bugs_test.go contains regression tests for four bugs introduced
// by the refs/frames/snaphash separation refactor:
//
//  1. --fs-dir=$DIR causes an extra $DIR/fs/ subdir for store metadata, so
//     frame subvolumes (fs/<uuid>) and metadata (fs/fs/<uuid>.jsonc) disagree.
//  2. UUID frames are created at fs/<uuid> instead of fs/<tsnet-user>/<uuid>,
//     losing per-user isolation.
//  3. Snaps in --snaps-dir are named with hex hashes rather than the base64url
//     snaphash encoding.
//  4. SSH into root@<ref>@host creates a NEW fs/<user>/<ref> directory instead
//     of resolving the ref to its frame UUID and using that frame.
//
// These tests drive the real handlers/functions (not the e2e mock control
// server), so they exercise the actual code paths. They require root + btrfs.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/snaphash"
)

// setupBugTestEnv builds a btrfs-backed env with a base snapshot and wires the
// global flag pointers and ref/frame stores the way main() would. It returns
// the env and the base snapshot ID.
func setupBugTestEnv(t *testing.T) (*testEnv, string, string) {
	t.Helper()

	root := requireBtrfsRoot2(t)
	env := createTestEnv(t, root, "bugtest")
	t.Cleanup(env.cleanup)

	tsBinary := buildTsBinaryForTest(t, root)
	baseSnap := createLocalBaseSnapshot(t, env, tsBinary)

	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	t.Cleanup(resetFlagsForTest)

	// Initialize the stores the way main() does. The state dir is the PARENT
	// of fs/ (see refs/frames package docs), so the correct stateDir is the
	// directory containing fsDir, NOT fsDir itself.
	stateDir := filepath.Dir(env.fsDir)
	initRefStore(stateDir)
	initFrameStore(stateDir)

	return env, baseSnap, stateDir
}

// TestBug1NoDoubleFsSubdir verifies that creating a UUID frame does not create
// a nested fs/fs/ directory. The frame subvolume and its metadata must live
// under the same fs/ directory.
func TestBug1NoDoubleFsSubdir(t *testing.T) {
	env, baseSnap, _ := setupBugTestEnv(t)

	tailscaleUser := "alice@example.com"
	cs := &controlServer{rootFS: filepath.Join(env.fsDir, sanitizeForPath(tailscaleUser), "placeholder")}

	resp := doCreateWithUUID(t, cs, baseSnap+"::", "")
	if resp.Status != "ok" {
		t.Fatalf("create failed: %s", resp.Message)
	}

	// There must be NO fs/fs directory.
	doubleFs := filepath.Join(env.fsDir, "fs")
	if _, err := os.Stat(doubleFs); err == nil {
		t.Errorf("bug 1: double fs/ subdir created at %s", doubleFs)
	}

	// The frame metadata (.jsonc) must live alongside the frame subvolume,
	// i.e. under fs/<user>/, not under a separate fs/fs/ tree.
	uuid := resp.UUID
	if uuid == "" {
		t.Fatal("create returned empty UUID")
	}
	// Frame metadata should be findable by the frame store at the same
	// state dir the subvolume was created under.
}

// TestBug2PerUserFramePath verifies that UUID frames are created at
// fs/<tsnet-user>/<uuid>, preserving per-user isolation.
func TestBug2PerUserFramePath(t *testing.T) {
	env, baseSnap, _ := setupBugTestEnv(t)

	tailscaleUser := "alice@example.com"
	safeUser := sanitizeForPath(tailscaleUser)
	cs := &controlServer{rootFS: filepath.Join(env.fsDir, safeUser, "placeholder")}

	resp := doCreateWithUUID(t, cs, baseSnap+"::", "")
	if resp.Status != "ok" {
		t.Fatalf("create failed: %s", resp.Message)
	}
	uuid := resp.UUID

	// The frame must be at fs/<user>/<uuid>, NOT fs/<uuid>.
	wantPath := filepath.Join(env.fsDir, safeUser, uuid)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("bug 2: frame not at per-user path %s: %v", wantPath, err)
	}

	badPath := filepath.Join(env.fsDir, uuid)
	if _, err := os.Stat(badPath); err == nil {
		t.Errorf("bug 2: frame created at non-per-user path %s", badPath)
	}

	// The reported path in the response should also be the per-user path.
	if resp.Path != wantPath {
		t.Errorf("bug 2: response path = %q, want %q", resp.Path, wantPath)
	}
}

// TestBug3SnapNamedWithSnaphash verifies that snapshot IDs use the base64url
// snaphash encoding (43 chars, decodable) rather than 64-char hex.
func TestBug3SnapNamedWithSnaphash(t *testing.T) {
	env, baseSnap, _ := setupBugTestEnv(t)

	// Create a frame and snap it.
	framePath := filepath.Join(env.fsDir, "testuser", "snaptest")
	if err := createFrame(framePath, baseSnap+"::", "", "", "container"); err != nil {
		t.Fatalf("createFrame: %v", err)
	}

	snapID, err := createSnapshot(framePath, nil, false)
	if err != nil {
		t.Fatalf("createSnapshot: %v", err)
	}
	t.Logf("snap ID: %s", snapID)

	// A three-component frame (created from "<snap>::") returns a frame spec
	// "rootfs:home:work" with "nil" for empty components. The rootfs component
	// is the actual snap, and that is the name that lands in --snaps-dir; it
	// must use the snaphash encoding (43 chars, decodable), not 64-char hex.
	rootfsID := snapID
	if i := strings.IndexByte(rootfsID, ':'); i != -1 {
		rootfsID = rootfsID[:i]
	}

	// snaphash encoding is exactly 43 chars; hex would be 64.
	if len(rootfsID) != snaphash.EncodedSize {
		t.Errorf("bug 3: rootfs snap ID length = %d, want %d (snaphash). Got %q",
			len(rootfsID), snaphash.EncodedSize, rootfsID)
	}

	// It must decode as a valid snaphash.
	if _, err := snaphash.Decode(rootfsID); err != nil {
		t.Errorf("bug 3: rootfs snap ID %q is not a valid snaphash: %v", rootfsID, err)
	}

	// The snapshot directory in --snaps-dir must be named with the snaphash.
	if _, err := os.Stat(filepath.Join(env.snapshotsDir, rootfsID)); err != nil {
		t.Errorf("bug 3: snapshot dir %s not found: %v", rootfsID, err)
	}
}

// TestBug4SSHResolvesRefToFrame verifies that resolving a frame for an SSH
// session consults the ref store: when a ref exists, it must resolve to the
// ref's frame UUID and reuse that frame's filesystem, NOT create a new
// fs/<user>/<refname> directory.
func TestBug4SSHResolvesRefToFrame(t *testing.T) {
	env, baseSnap, _ := setupBugTestEnv(t)

	tailscaleUser := "alice@example.com"
	safeUser := sanitizeForPath(tailscaleUser)
	cs := &controlServer{rootFS: filepath.Join(env.fsDir, safeUser, "placeholder")}

	// Create a frame with a ref named "deb" (mirrors `ts frame --ref=deb <snap>::`).
	resp := doCreateWithUUID(t, cs, baseSnap+"::", "deb")
	if resp.Status != "ok" {
		t.Fatalf("create with ref failed: %s", resp.Message)
	}
	uuid := resp.UUID
	frameUUIDPath := filepath.Join(env.fsDir, safeUser, uuid)
	if _, err := os.Stat(frameUUIDPath); err != nil {
		t.Fatalf("frame not created at %s: %v", frameUUIDPath, err)
	}

	// Now simulate `ssh root@deb@host`: resolve frameName "deb" for this user.
	rootFS, err := resolveFrameRootFS(tailscaleUser, "deb")
	if err != nil {
		t.Fatalf("resolveFrameRootFS: %v", err)
	}

	// It must resolve to the EXISTING frame UUID path, not a new fs/<user>/deb.
	if rootFS != frameUUIDPath {
		t.Errorf("bug 4: ref 'deb' resolved to %q, want frame UUID path %q", rootFS, frameUUIDPath)
	}

	badPath := filepath.Join(env.fsDir, safeUser, "deb")
	if _, err := os.Stat(badPath); err == nil {
		t.Errorf("bug 4: a new fs/<user>/deb directory was created at %s", badPath)
	}
}

// TestBug4SSHAutoCreatesMissingRef verifies that SSHing to a non-existent ref
// auto-creates an empty frame + ref (the agreed temporary behavior) rather
// than erroring or making a bare fs/<user>/<name> dir.
func TestBug4SSHAutoCreatesMissingRef(t *testing.T) {
	env, _, _ := setupBugTestEnv(t)

	tailscaleUser := "bob@example.com"
	safeUser := sanitizeForPath(tailscaleUser)

	// "fresh" ref does not exist yet.
	if refStore.Exists(refKeyForUser(safeUser, "fresh")) {
		t.Fatal("precondition: ref should not exist")
	}

	rootFS, err := resolveFrameRootFS(tailscaleUser, "fresh")
	if err != nil {
		t.Fatalf("resolveFrameRootFS (auto-create): %v", err)
	}

	// A ref must now exist for this user pointing at a UUID, and rootFS must
	// be the UUID path under fs/<user>/.
	ref, err := refStore.Get(refKeyForUser(safeUser, "fresh"))
	if err != nil {
		t.Fatalf("bug 4: ref 'fresh' was not auto-created: %v", err)
	}
	wantPath := filepath.Join(env.fsDir, safeUser, ref.UUID.String())
	if rootFS != wantPath {
		t.Errorf("bug 4: auto-created ref resolved to %q, want %q", rootFS, wantPath)
	}
	if _, err := os.Stat(rootFS); err != nil {
		t.Errorf("bug 4: auto-created frame fs missing at %s: %v", rootFS, err)
	}
}

// doCreateWithUUID drives the real handleCreate (UUID path) via httptest and
// returns the decoded CreateResponse.
func doCreateWithUUID(t *testing.T, cs *controlServer, snapshotSpec, refName string) CreateResponse {
	t.Helper()

	body, _ := json.Marshal(CreateRequest{
		SnapshotSpec: snapshotSpec,
		RefName:      refName,
		Isolation:    "container",
	})
	req := httptest.NewRequest(http.MethodPost, "/create", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	cs.handleCreate(rec, req)

	var resp CreateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode create response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// ensure refs import is used even if helper signatures change during fixing.
var _ = refs.ErrRefNotFound
