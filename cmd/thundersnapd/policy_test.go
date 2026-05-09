package main

import (
	"os"
	"path/filepath"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func TestMatchesSrc(t *testing.T) {
	tests := []struct {
		name      string
		patterns  []string
		loginName string
		tags      []string
		want      bool
	}{
		{
			name:      "exact user match",
			patterns:  []string{"alice@example.com"},
			loginName: "alice@example.com",
			tags:      nil,
			want:      true,
		},
		{
			name:      "user no match",
			patterns:  []string{"alice@example.com"},
			loginName: "bob@example.com",
			tags:      nil,
			want:      false,
		},
		{
			name:      "autogroup member matches user",
			patterns:  []string{"autogroup:member"},
			loginName: "alice@example.com",
			tags:      nil,
			want:      true,
		},
		{
			name:      "autogroup member no match for empty",
			patterns:  []string{"autogroup:member"},
			loginName: "",
			tags:      nil,
			want:      false,
		},
		{
			name:      "tag match",
			patterns:  []string{"tag:ci-worker"},
			loginName: "",
			tags:      []string{"tag:ci-worker", "tag:other"},
			want:      true,
		},
		{
			name:      "tag no match",
			patterns:  []string{"tag:admin"},
			loginName: "",
			tags:      []string{"tag:ci-worker"},
			want:      false,
		},
		{
			name:      "multiple patterns user match",
			patterns:  []string{"alice@example.com", "bob@example.com"},
			loginName: "bob@example.com",
			tags:      nil,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesSrc(tt.patterns, tt.loginName, tt.tags)
			if got != tt.want {
				t.Errorf("matchesSrc() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicyFileMatchGrant(t *testing.T) {
	policy := &PolicyFile{
		Grants: []PolicyGrant{
			{
				Src: []string{"alice@example.com"},
				Dst: []string{"tag:thundersnap"},
				App: map[string][]ThundersnapCap{
					ThundersnapCapName: {{
						Role:          "admin",
						Isolation:     "none",
						MaxFrames: -1,
					}},
				},
			},
			{
				Src: []string{"tag:ci-worker"},
				Dst: []string{"tag:thundersnap"},
				App: map[string][]ThundersnapCap{
					ThundersnapCapName: {{
						Role:          "ephemeral",
						Isolation:     "container",
						MaxFrames: 50,
						Ephemeral:     true,
					}},
				},
			},
			{
				Src: []string{"autogroup:member"},
				Dst: []string{"tag:thundersnap"},
				App: map[string][]ThundersnapCap{
					ThundersnapCapName: {{
						Role:          "developer",
						Isolation:     "vm",
						MaxFrames: 10,
					}},
				},
			},
		},
	}

	tests := []struct {
		name      string
		loginName string
		tags      []string
		wantRole  string
		wantIso   string
		wantNil   bool
	}{
		{
			name:      "alice gets admin",
			loginName: "alice@example.com",
			tags:      nil,
			wantRole:  "admin",
			wantIso:   "none",
		},
		{
			name:      "ci-worker gets ephemeral",
			loginName: "",
			tags:      []string{"tag:ci-worker"},
			wantRole:  "ephemeral",
			wantIso:   "container",
		},
		{
			name:      "other user gets developer",
			loginName: "bob@example.com",
			tags:      nil,
			wantRole:  "developer",
			wantIso:   "vm",
		},
		{
			name:      "no match returns nil",
			loginName: "",
			tags:      nil,
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := policy.MatchGrant(tt.loginName, tt.tags)
			if tt.wantNil {
				if cap != nil {
					t.Errorf("MatchGrant() = %+v, want nil", cap)
				}
				return
			}
			if cap == nil {
				t.Fatal("MatchGrant() = nil, want non-nil")
			}
			if cap.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", cap.Role, tt.wantRole)
			}
			if cap.Isolation != tt.wantIso {
				t.Errorf("Isolation = %q, want %q", cap.Isolation, tt.wantIso)
			}
		})
	}
}

func TestLoadPolicyFile(t *testing.T) {
	tmpDir := t.TempDir()
	policyPath := filepath.Join(tmpDir, "policy.jsonc")

	content := `{
  // This is a comment
  "grants": [
    {
      "src": ["autogroup:member"],
      "dst": ["tag:thundersnap"],
      "app": {
        "tailscale.com/cap/thundersnap": [{
          "role": "developer",
          "isolation": "vm",
          "maxFrames": 10,
        }],
      },
    },
  ],
}
`
	if err := os.WriteFile(policyPath, []byte(content), 0644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	policy, err := LoadPolicyFile(policyPath)
	if err != nil {
		t.Fatalf("LoadPolicyFile: %v", err)
	}

	if len(policy.Grants) != 1 {
		t.Errorf("len(Grants) = %d, want 1", len(policy.Grants))
	}

	cap := policy.MatchGrant("alice@example.com", nil)
	if cap == nil {
		t.Fatal("MatchGrant returned nil")
	}
	if cap.Role != "developer" {
		t.Errorf("Role = %q, want developer", cap.Role)
	}
}

func TestResolveCap(t *testing.T) {
	policy := &PolicyFile{
		Grants: []PolicyGrant{
			{
				Src: []string{"alice@example.com"},
				App: map[string][]ThundersnapCap{
					ThundersnapCapName: {{Role: "admin", Isolation: "none"}},
				},
			},
		},
	}

	tests := []struct {
		name      string
		who       *apitype.WhoIsResponse
		policy    *PolicyFile
		wantRole  string
		wantIso   string
	}{
		{
			name:     "nil who uses default",
			who:      nil,
			policy:   nil,
			wantRole: "developer",
			wantIso:  "container",
		},
		{
			name: "policy match",
			who: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com"},
			},
			policy:   policy,
			wantRole: "admin",
			wantIso:  "none",
		},
		{
			name: "no policy match uses default",
			who: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "bob@example.com"},
			},
			policy:   policy,
			wantRole: "developer",
			wantIso:  "container",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := ResolveCap(tt.who, tt.policy)
			if cap.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", cap.Role, tt.wantRole)
			}
			if cap.Isolation != tt.wantIso {
				t.Errorf("Isolation = %q, want %q", cap.Isolation, tt.wantIso)
			}
		})
	}
}

func TestFillDefaults(t *testing.T) {
	tests := []struct {
		name  string
		input ThundersnapCap
		want  ThundersnapCap
	}{
		{
			name:  "empty gets all defaults",
			input: ThundersnapCap{},
			want:  DefaultCap,
		},
		{
			name:  "partial preserves set values",
			input: ThundersnapCap{Role: "admin", Isolation: "none"},
			want:  ThundersnapCap{Role: "admin", Isolation: "none", MaxFrames: 5},
		},
		{
			name:  "full stays unchanged",
			input: ThundersnapCap{Role: "admin", Isolation: "none", MaxFrames: -1, Ephemeral: true},
			want:  ThundersnapCap{Role: "admin", Isolation: "none", MaxFrames: -1, Ephemeral: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fillDefaults(tt.input)
			if got != tt.want {
				t.Errorf("fillDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
