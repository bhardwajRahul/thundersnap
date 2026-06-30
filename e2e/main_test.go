//go:build e2e

package e2e

import (
	"flag"
	"os"
	"testing"
)

// TestMain is the entry point for the e2e test suite.
// These are true end-to-end tests that start a real thundersnapd and
// connect to it over SSH.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}
