// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// suite_test.go groups the e2e scenario helpers (the lowercase testX
// functions spread across the other e2e/*.go files) into the smallest
// possible number of top-level Go tests.
//
// Each top-level test here spins up exactly one thundersnapd and keeps it alive
// for the whole test, running every scenario as a t.Run subtest against that
// single daemon. This replaces the old layout where every scenario was its own
// top-level TestX that paid for a full daemon (and, for VM scenarios,
// cloud-hypervisor/passt/virtiofsd) spin-up/down. The assertions are unchanged
// — only their grouping is — so the suite gets the same coverage with a handful
// of daemon lifecycles instead of one per scenario. A single long-lived daemon
// is also a better race/edge-case detector: background snap workers, ref
// stores, and per-frame state are exercised continuously instead of being torn
// down and rebuilt every few seconds.
//
// There are deliberately three separate top-level tests, one per isolation
// mode, because each mode needs a different daemon setup:
//
//   - TestContainer: container-isolation policy (the default).
//   - TestVM: vmx-isolation policy (boots cloud-hypervisor VMs).
//   - TestNestedThundersnap: defined in nested_test.go; runs a daemon inside a
//     daemon and needs its own outer daemon + host-side binary copying.
//
// Subtests are addressable with -test.run (e.g.
// `make e2e E2E_ARGS="-test.run=TestContainer/FrameCreateDelete"`), so the
// single-scenario debugging workflow still works: the shared daemon starts once
// and only the named subtest runs.
//
// REF-NAME UNIQUENESS: every scenario shares its daemon's single ref namespace,
// so a ref name created by one scenario must not be reused by another (a second
// `ts ref create`/`ts frame --ref=` for an existing name fails, and a `root@X`
// in one scenario would silently resolve to another scenario's frame). Audit
// new scenarios against the existing ref names before adding them.
package e2e

import "testing"

// TestContainer runs every container-isolation e2e scenario as a subtest of a
// single shared thundersnapd. Scenarios are ordered simplest-to-hardest so that
// under -test.failfast a foundational breakage (SSH, namespaces) is reported
// first instead of a cascade of downstream failures.
func TestContainer(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	scenarios := []struct {
		name string
		fn   func(*testing.T, *daemonInstance)
	}{
		// Foundational session behavior: if these fail, the rest is moot.
		{"FrameForkCreatesRef", testFrameForkCreatesRef},
		{"SSHContainerBasic", testSSHContainerBasic},
		{"SessionEnvVars", testSessionEnvVars},
		{"SSHContainerUserRoot", testSSHContainerUserRoot},
		{"SSHContainerUserNonRoot", testSSHContainerUserNonRoot},
		{"SSHContainerWorkingDir", testSSHContainerWorkingDir},
		{"SSHContainerStdoutStderrSeparate", testSSHContainerStdoutStderrSeparate},
		{"ContainerNamespaceSetup", testContainerNamespaceSetup},
		{"ContainerIsolationOverSSH", testContainerIsolationOverSSH},

		// PTY relay invariants (echo, winsize, write ordering, raw mode, pts).
		{"ContainerNonRootPtyJobControl", testContainerNonRootPtyJobControl},
		{"ContainerPtyEcho", testContainerPtyEcho},
		{"ContainerPtyWinsize", testContainerPtyWinsize},
		{"ContainerPtyWriteOrder", testContainerPtyWriteOrder},
		{"ContainerPtyRawNoCRInjection", testSSHContainerPtyRawNoCRInjection},
		{"ContainerConcurrentDistinctPTS", testContainerConcurrentDistinctPTS},

		// Frame lifecycle: create/delete, refs, snap+fork content preservation.
		{"FrameCreateDelete", testFrameCreateDelete},
		{"FrameDeleteRequiresAllRefsRemoved", testFrameDeleteRequiresAllRefsRemoved},
		{"FrameFromSnapPreservesContent", testFrameFromSnapPreservesContent},
		{"FrameHomeWorkSpec", testFrameHomeWorkSpec},
		{"CrossFrameWorkSharing", testCrossFrameWorkSharing},
		{"FrameFromNonExistentSnap", testFrameFromNonExistentSnap},
		{"DeleteRunningFrame", testDeleteRunningFrame},
		{"SnapDeleteSucceedsAndFrameIntact", testSnapDeleteSucceedsAndFrameIntact},
		{"RefMoveAndForceDelete", testRefMoveAndForceDelete},
		{"FrameUserAndGroup", testFrameUserAndGroup},
		{"FrameHomeWorkSymlink", testFrameHomeWorkSymlink},
		{"FrameIdNotCloned", testFrameIdNotCloned},
		{"FrameStatePersistsAcrossSessions", testFrameStatePersistsAcrossSessions},
		{"SnapManyFiles", testSnapManyFiles},

		// ts frame / ts go / ts undo command surface.
		{"TsFrame", testTsFrame},
		{"TsLog", testTsLog},
		{"TsFrameCreatesNewFrame", testTsFrameCreatesNewFrame},
		{"TsFrameColonColonCreatesNewFrame", testTsFrameColonColonCreatesNewFrame},
		{"TsGoNoArgsCreatesThenEnters", testTsGoNoArgsCreatesThenEnters},
		{"TsGoWithCommand", testTsGoWithCommand},
		{"TsGoWithCommandExitCode", testTsGoWithCommandExitCode},
		{"TsGoWithCommandToExistingFrame", testTsGoWithCommandToExistingFrame},
		{"TsGoUserAtFrame", testTsGoUserAtFrame},
		{"TsUndo", testTsUndo},
		{"TsUndoEmptyLog", testTsUndoEmptyLog},
		{"ForkUndoRollsBackToForkPoint", testForkUndoRollsBackToForkPoint},

		// Snapshot fidelity (ownership, setuid/setgid, hardlinks, symlinks).
		{"SnapFidelityPreservesSpecialFiles", testSnapFidelityPreservesSpecialFiles},

		// Taint add/list/dedup and propagation.
		{"TaintAddListDedup", testTaintAddListDedup},
		{"TaintPropagatesThroughFork", testTaintPropagatesThroughFork},
		{"TaintSurvivesBackgroundSnap", testTaintSurvivesBackgroundSnap},

		// Background (fire-and-forget) snap indexing.
		{"SnapBackgroundIndexing", testSnapBackgroundIndexing},
		{"SnapBackgroundCapturesAtCallTime", testSnapBackgroundCapturesAtCallTime},
		{"SnapRapidDoubleBackgroundIndexing", testSnapRapidDoubleBackgroundIndexing},

		// Error / negative cases.
		{"ErrorSSHUnknownFrame", testErrorSSHUnknownFrame},
		{"ErrorDeleteNonexistentFrame", testErrorDeleteNonexistentFrame},
		{"ErrorSnapSymlinkLoop", testErrorSnapSymlinkLoop},
		{"ErrorSSHUnknownUser", testErrorSSHUnknownUser},
		{"ErrorInvalidFrameSpec", testErrorInvalidFrameSpec},
		{"ErrorInvalidFrameName", testErrorInvalidFrameName},

		// Autorun: config storage, process start/stop/restart, ref move.
		{"AutorunBasic", testAutorunBasic},
		{"AutorunRefMove", testAutorunRefMove},
		{"AutorunStop", testAutorunStop},
		{"AutorunWithNonExistentRef", testAutorunWithNonExistentRef},
		{"AutorunShowsInRefs", testAutorunShowsInRefs},
		{"AutorunMultiWordCommand", testAutorunMultiWordCommand},
		{"AutorunProcessStarts", testAutorunProcessStarts},
		{"AutorunProcessStops", testAutorunProcessStops},
		{"AutorunProcessRestartsOnRefMove", testAutorunProcessRestartsOnRefMove},
		{"AutorunProcessAutoRestart", testAutorunProcessAutoRestart},

		// Cross-instance snap determinism. This is the one scenario that needs
		// a SECOND independent daemon; it reuses this test's daemon as d1 and
		// spins up its own d2 (separate state dir) inside the subtest. Run last
		// so the extra daemon only lives for the scenario that needs it.
		{"CrossInstanceSnapDeterminism", testCrossInstanceSnapDeterminism},

		// Mesh who-has + download-snap between two independent HTTP-enabled
		// daemons. Like CrossInstanceSnapDeterminism it starts its own pair of
		// daemons (both with --test-http-listen); the suite's shared daemon is
		// unused. Runs near the end since it pays for two extra daemons.
		{"MeshWhoHasAndDownload", testMeshWhoHasAndDownload},

		// Destructive by design: this must remain last because it SIGKILLs the
		// shared daemon while sessions and autorun processes are still alive.
		{"CrashLifecycle", testContainerCrashLifecycle},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) { sc.fn(t, d) })
	}
}

// TestVM runs every VM (vmx) isolation scenario as a subtest of a single shared
// thundersnapd started with a vmx policy (startVMDaemon, which also checks VM
// deps: cloud-hypervisor, vmlinux, virtiofsd, passt, /dev/kvm). See
// TestContainer for the rationale and the ref-name uniqueness rule.
func TestVM(t *testing.T) {
	env := newTestEnv(t)
	d := startVMDaemon(t, env)

	scenarios := []struct {
		name string
		fn   func(*testing.T, *daemonInstance)
	}{
		{"VMSSHSessionMatrix", testVMSSHSessionMatrix},
		{"VMNamespaceSetup", testVMNamespaceSetup},
		{"VMDeepWorkflow", testVMDeepWorkflow},
		{"VMXPtyWinsize", testVMXPtyWinsize},

		// Destructive by design: this must remain last because it SIGKILLs the
		// shared daemon while a VM session is still alive.
		{"CrashLifecycle", testVMCrashLifecycle},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) { sc.fn(t, d) })
	}
}
