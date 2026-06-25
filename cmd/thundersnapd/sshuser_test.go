package main

import "testing"

// TestParseSSHUser tests the SSH username parsing logic.
func TestParseSSHUser(t *testing.T) {
	tests := []struct {
		name           string
		sshUser        string
		wantIsolation  string
		wantVMXIso     string // vmxIsolation value
		wantTargetUser string
		wantFrameName  string
	}{
		// Basic frame name (container mode)
		{
			name:          "simple frame",
			sshUser:       "myframe",
			wantIsolation: "container",
			wantFrameName: "myframe",
		},
		{
			name:           "frame with user prefix",
			sshUser:        "ubuntu@myframe",
			wantIsolation:  "container",
			wantTargetUser: "ubuntu",
			wantFrameName:  "myframe",
		},

		// VMX mode: vmx/<isolation>/<frame>
		{
			name:          "vmx with isolation and frame",
			sshUser:       "vmx/dev/frame1",
			wantIsolation: "vmx",
			wantVMXIso:    "dev",
			wantFrameName: "frame1",
		},
		{
			name:          "vmx isolation only (outer shell)",
			sshUser:       "vmx/dev",
			wantIsolation: "vmx",
			wantVMXIso:    "dev",
			wantFrameName: "",
		},
		{
			name:           "vmx with user in frame",
			sshUser:        "vmx/dev/ubuntu@frame1",
			wantIsolation:  "vmx",
			wantVMXIso:     "dev",
			wantTargetUser: "ubuntu",
			wantFrameName:  "frame1",
		},
		// BUG TEST: user@ prefix before vmx/
		{
			name:           "user prefix before vmx",
			sshUser:        "root@vmx/b/deb",
			wantIsolation:  "vmx",
			wantVMXIso:     "b",
			wantTargetUser: "root",
			wantFrameName:  "deb",
		},
		{
			name:           "user prefix before vmx outer shell",
			sshUser:        "root@vmx/dev",
			wantIsolation:  "vmx",
			wantVMXIso:     "dev",
			wantTargetUser: "root",
			wantFrameName:  "",
		},

		// Legacy VM mode: vm/<frame> -> vmx/default/<frame>
		{
			name:          "legacy vm mode",
			sshUser:       "vm/myframe",
			wantIsolation: "vmx",
			wantVMXIso:    "default",
			wantFrameName: "myframe",
		},
		{
			name:           "legacy vm with user in frame",
			sshUser:        "vm/ubuntu@myframe",
			wantIsolation:  "vmx",
			wantVMXIso:     "default",
			wantTargetUser: "ubuntu",
			wantFrameName:  "myframe",
		},
		// BUG TEST: user@ prefix before vm/
		{
			name:           "user prefix before vm",
			sshUser:        "root@vm/deb",
			wantIsolation:  "vmx",
			wantVMXIso:     "default",
			wantTargetUser: "root",
			wantFrameName:  "deb",
		},

		// Edge cases
		{
			name:          "vmx with empty frame after slash",
			sshUser:       "vmx/dev/",
			wantIsolation: "vmx",
			wantVMXIso:    "dev",
			wantFrameName: "",
		},
		{
			name:          "empty input",
			sshUser:       "",
			wantIsolation: "container",
			wantFrameName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolation, vmxIso, targetUser, frameName := parseSSHUser(tt.sshUser)

			if isolation != tt.wantIsolation {
				t.Errorf("isolation = %q, want %q", isolation, tt.wantIsolation)
			}
			if vmxIso != tt.wantVMXIso {
				t.Errorf("vmxIsolation = %q, want %q", vmxIso, tt.wantVMXIso)
			}
			if targetUser != tt.wantTargetUser {
				t.Errorf("targetUser = %q, want %q", targetUser, tt.wantTargetUser)
			}
			if frameName != tt.wantFrameName {
				t.Errorf("frameName = %q, want %q", frameName, tt.wantFrameName)
			}
		})
	}
}
