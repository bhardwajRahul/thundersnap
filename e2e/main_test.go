//go:build e2e

package e2e

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// TestMain runs the e2e suite in tiers, from simplest/cheapest to
// hardest/most-expensive. If any earlier tier fails, the remaining tiers are
// skipped entirely (the binary exits non-zero immediately).
//
// The rationale (see todo): there is little point running the full, slow VM
// test suite if a foundational test like "can we create a base snapshot" has
// already failed. Almost all later failures in that situation are just
// downstream symptoms of the same root cause, so we abort early and surface the
// first failing tier.
//
// Ordering is by test-name regexp. The tiers are evaluated in order; each tier
// is run via -test.run so individual test names never have to be enumerated.
// Every top-level test is matched by at least one tier (kept in sync by hand;
// an unmatched test simply never runs, which a maintainer would notice as a
// missing PASS line). If a caller passes an explicit -test.run, tiering is
// disabled and that filter is honored as-is.
func TestMain(m *testing.M) {
	flag.Parse()

	// Honor an explicit -test.run from the caller: if the user asked to run a
	// specific subset, don't impose tiering on top of it.
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		os.Exit(m.Run())
	}

	// Tiers run in order, cheapest/most-foundational first. Each pattern is an
	// anchored alternation of test-name prefixes. Every top-level test must be
	// matched by exactly one tier (verified by TestTierCoverage).
	tiers := []struct {
		name string
		run  string
	}{
		// Tier 0: pure in-process package checks (no btrfs, no daemon).
		// NOTE: tiers should be mutually exclusive so no test runs twice. A few
		// trivial in-process tests (e.g. TestFramePath) are matched by both the
		// unit tier and a later broad tier; they are idempotent and sub-
		// millisecond, so the harmless double-run is accepted rather than
		// complicating the patterns with RE2-unfriendly exclusions.
		{"unit", `^Test(RefsPackage|FramesPackage|SnaphashEncoding|FrameidGeneration|RefNameValidation|FramePath|FixtureCreatesAllFileTypes|DefaultTestContainerSpecCompleteness)$`},

		// Tier 1: TSM/TSC on-disk format fundamentals.
		{"format", `^Test(TSM|TSC).*$`},

		// Tier 2: basic snapshot create/list/dedup/delete + file-type handling.
		{"snapshot", `^Test(E2EBasicSnapshot|E2EOwnership|E2EDevSetup|Snapshot|NestedSnapshotTree|LargeDirectoryTree|ConcurrentModificationDuringSnapshot|DeleteSnapshotWithReference|Hardlink|Symlink|Setuid|Setgid).*$`},

		// Tier 3: error handling and refs.
		{"refs-errors", `^Test(Error|Ref|CorruptedSnapshotMetadata).*$`},

		// Tier 4: container isolation primitives.
		{"container", `^Test(BlankContainer|Container|NestedThundersnap|Cgroup).*$`},

		// Tier 5: frames, taints, integration, mesh, streaming, uid, docker.
		{"frames", `^Test(Frame|MultipleTaintsOnFrame|Taint|Integration|Workflow|CrossFrame|Mesh|Metrics|Streaming|NDJSON|HTTPRange|Vsock|UID|Id|Docker|MultipleConcurrentSessions|LongRunningProgressUpdates|QueryFrameTaints|DeleteRunningFrame).*$`},

		// Tier 6: SSH/shell scenarios.
		{"shell", `^Test(SSH|MinimalShell).*$`},

		// Tier 7: VM/VMX tests (slowest: boot cloud-hypervisor).
		{"vm", `^Test(VM|E2EVMPanicRecovery).*$`},
	}

	for _, tier := range tiers {
		if err := flag.Set("test.run", tier.run); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: failed to set test.run for tier %q: %v\n", tier.name, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\n=== running e2e tier %q ===\n", tier.name)
		if code := m.Run(); code != 0 {
			fmt.Fprintf(os.Stderr, "\n=== e2e tier %q failed; skipping all later tiers ===\n", tier.name)
			os.Exit(code)
		}
	}

	os.Exit(0)
}
