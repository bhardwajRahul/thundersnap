// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// policy.go implements the grant-based policy system for thundersnap.
// Policies can come from Tailscale CapMap grants or a local policy file.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tailscale/hujson"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// ThundersnapCapName is the capability name used in Tailscale grants.
const ThundersnapCapName = "tailscale.com/cap/thundersnap"

// defaultNamespace is used for every principal not explicitly mapped by the
// local policy. Frames and refs are keyed by namespace, not by the connecting
// Tailscale identity.
const defaultNamespace = "shared"

// ThundersnapCap represents the grant capability structure.
type ThundersnapCap struct {
	// Role determines the session type.
	// "developer" (default), "admin", "ephemeral", "service"
	Role string `json:"role,omitempty"`

	// Isolation determines the execution environment.
	// "vm" (default): user gets a dedicated VM, containers inside it
	// "container": direct chroot container on the host (no VM)
	// "none": no sub-isolation (single-user thundersnap instance)
	Isolation string `json:"isolation,omitempty"`

	// MaxFrames limits how many concurrent frames the user can have.
	// -1 means unlimited. Default: 5.
	MaxFrames int `json:"maxFrames,omitempty"`

	// Ephemeral means frames are deleted when the last session disconnects.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// DefaultCap is the default capability when no grant matches.
var DefaultCap = ThundersnapCap{
	Role:      "developer",
	Isolation: "container",
	MaxFrames: 5,
	Ephemeral: false,
}

// PolicyFile represents the local policy file structure.
// It uses the same grant format as Tailscale policy files.
type PolicyFile struct {
	Grants []PolicyGrant `json:"grants"`

	// Namespaces maps authenticated principal names to workspace namespaces.
	// Principals absent from this map use the "shared" namespace. A namespace
	// is a single directory name below fs/, refs/, and id/.
	Namespaces map[string]string `json:"namespaces,omitempty"`
}

// PolicyGrant represents a single grant in the policy file.
type PolicyGrant struct {
	Src []string                    `json:"src"` // user identities or tags
	Dst []string                    `json:"dst"` // ignored locally (for Tailscale compat)
	App map[string][]ThundersnapCap `json:"app"`
}

// LoadPolicyFile loads and parses a local policy file (hujson format).
func LoadPolicyFile(path string) (*PolicyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Standardize hujson to JSON
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse hujson: %w", err)
	}

	var policy PolicyFile
	if err := json.Unmarshal(standardized, &policy); err != nil {
		return nil, fmt.Errorf("unmarshal policy: %w", err)
	}
	for principal, namespace := range policy.Namespaces {
		if principal == "" {
			return nil, fmt.Errorf("namespace mapping has an empty principal")
		}
		if err := validateNamespace(namespace); err != nil {
			return nil, fmt.Errorf("namespace for %q: %w", principal, err)
		}
	}

	return &policy, nil
}

// NamespaceForPrincipal returns the workspace namespace assigned to principal.
// Namespace mappings are local policy, rather than Tailscale capabilities:
// they choose where persistent state lives and do not grant access by
// themselves. Unmapped principals, including the temporary MCP principal
// "unknown", share the default namespace.
func (p *PolicyFile) NamespaceForPrincipal(principal string) string {
	if p != nil {
		if namespace := p.Namespaces[principal]; namespace != "" {
			return namespace
		}
	}
	return defaultNamespace
}

// validateNamespace ensures a namespace is exactly one safe path component.
// Email addresses remain valid, which permits explicitly selecting an old
// per-user directory during an early-install migration.
func validateNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("must not be empty")
	}
	if namespace == "." || namespace == ".." || strings.ContainsAny(namespace, "/\\\x00") {
		return fmt.Errorf("%q is not a valid directory name", namespace)
	}
	return nil
}

// MatchGrant finds the first matching grant for the given identity.
// loginName is the user's email (e.g., "alice@example.com").
// tags is the list of tags if connecting as a tagged node.
func (p *PolicyFile) MatchGrant(loginName string, tags []string) *ThundersnapCap {
	for _, grant := range p.Grants {
		if matchesSrc(grant.Src, loginName, tags) {
			if caps, ok := grant.App[ThundersnapCapName]; ok && len(caps) > 0 {
				return &caps[0]
			}
		}
	}
	return nil
}

// matchesSrc checks if the identity matches any of the src patterns.
func matchesSrc(patterns []string, loginName string, tags []string) bool {
	for _, pattern := range patterns {
		// Exact user match
		if pattern == loginName {
			return true
		}

		// Autogroup match
		if pattern == "autogroup:member" {
			// Match any authenticated user
			if loginName != "" && !strings.HasPrefix(loginName, "tag:") {
				return true
			}
		}

		// Tag match
		for _, tag := range tags {
			if pattern == tag {
				return true
			}
		}
	}
	return false
}

// GetCapFromWhoIs extracts ThundersnapCap from a WhoIs response's CapMap.
// Returns nil if no thundersnap capability is present.
func GetCapFromWhoIs(who *apitype.WhoIsResponse) (*ThundersnapCap, error) {
	if who == nil || who.CapMap == nil {
		return nil, nil
	}

	caps, err := tailcfg.UnmarshalCapJSON[ThundersnapCap](
		who.CapMap, ThundersnapCapName,
	)
	if err != nil {
		return nil, fmt.Errorf("unmarshal cap: %w", err)
	}

	if len(caps) == 0 {
		return nil, nil
	}

	// Return first cap (could merge multiple in future)
	return &caps[0], nil
}

// ResolveCap resolves the effective capability for a connection.
// Priority: CapMap grant > local policy file > hardcoded default.
func ResolveCap(who *apitype.WhoIsResponse, policy *PolicyFile) ThundersnapCap {
	// Try CapMap first (highest priority)
	if who != nil {
		cap, err := GetCapFromWhoIs(who)
		if err == nil && cap != nil {
			return fillDefaults(*cap)
		}
	}

	// Try local policy file
	if policy != nil && who != nil {
		loginName := ""
		var tags []string

		if who.UserProfile != nil {
			loginName = who.UserProfile.LoginName
		}
		if who.Node != nil {
			tags = who.Node.Tags
		}

		cap := policy.MatchGrant(loginName, tags)
		if cap != nil {
			return fillDefaults(*cap)
		}
	}

	// Fall back to default
	return DefaultCap
}

// fillDefaults fills in default values for unset fields.
func fillDefaults(cap ThundersnapCap) ThundersnapCap {
	if cap.Role == "" {
		cap.Role = DefaultCap.Role
	}
	if cap.Isolation == "" {
		cap.Isolation = DefaultCap.Isolation
	}
	if cap.MaxFrames == 0 {
		cap.MaxFrames = DefaultCap.MaxFrames
	}
	return cap
}
