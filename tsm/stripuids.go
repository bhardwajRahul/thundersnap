package tsm

// This file provides helpers to ensure a "user" account exists in /etc/passwd
// for container environments. Unlike the previous UID stripping approach,
// we now preserve all original UIDs from Docker images and snapshots.
//
// The "user" account uses UID/GID 7575 (matching the thundersnap HTTP port)
// which is unlikely to conflict with existing container users.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ThundersnapUID is the UID used for the "user" account created by thundersnap.
// 7575 matches the thundersnap HTTP port and is unlikely to conflict with
// existing container users.
const ThundersnapUID = 7575

// ThundersnapGID is the GID used for the "user" account.
const ThundersnapGID = 7575

// EnsureUserInPasswd ensures that a user named "user" exists in /etc/passwd
// (and /etc/shadow if it exists). If the user doesn't exist, it's added with:
//   - UID/GID: ThundersnapUID/ThundersnapGID (7575)
//   - Home: /home
//   - Shell: /bin/bash if available, otherwise /bin/sh
//
// Returns the home directory of the "user" account (from passwd, not filesystem).
// Returns empty string if /etc/passwd doesn't exist.
func EnsureUserInPasswd(rootfs string) (home string, err error) {
	passwdPath := filepath.Join(rootfs, "etc", "passwd")
	in, err := os.ReadFile(passwdPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}

	// Check if "user" already exists
	var lines []string
	var userExists bool
	var userHome string
	scanner := bufio.NewScanner(strings.NewReader(string(in)))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		entry := parsePasswdEntry(line)
		if entry != nil && entry.Username == "user" {
			userExists = true
			userHome = entry.Home
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan passwd: %w", err)
	}

	if userExists {
		return userHome, nil
	}

	// User doesn't exist - add it after root (first non-root line position)
	// Pick shell: prefer /bin/bash if it exists, otherwise /bin/sh
	shell := "/bin/sh"
	if _, err := os.Stat(filepath.Join(rootfs, "bin", "bash")); err == nil {
		shell = "/bin/bash"
	}

	newPasswdEntry := fmt.Sprintf("user:x:%d:%d:user:/home:%s",
		ThundersnapUID, ThundersnapGID, shell)

	// Insert after root line in passwd
	var out strings.Builder
	inserted := false
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
		// Insert after root entry (UID 0)
		if !inserted {
			entry := parsePasswdEntry(line)
			if entry != nil && entry.UID == 0 {
				out.WriteString(newPasswdEntry)
				out.WriteByte('\n')
				inserted = true
			}
		}
	}
	// If we never found root (weird but possible), append at end
	if !inserted {
		out.WriteString(newPasswdEntry)
		out.WriteByte('\n')
	}

	if err := atomicWriteFile(passwdPath, []byte(out.String()), 0644); err != nil {
		return "", err
	}

	// Also add to /etc/shadow if it exists
	if err := ensureUserInShadow(rootfs); err != nil {
		return "", err
	}

	// Also ensure a matching "user" group (GID 7575) exists in /etc/group.
	// Without this, the user's primary GID 7575 has no name, which breaks
	// tools that look up group names (e.g. `id`, `ls -l`).
	if err := ensureUserInGroup(rootfs); err != nil {
		return "", err
	}

	return "/home", nil
}

// ensureUserInGroup adds a "user" group with GID ThundersnapGID to /etc/group
// if the file exists and neither the "user" group name nor GID 7575 is already
// present. This matches the "user" account added to /etc/passwd, which uses
// GID 7575 as its primary group.
func ensureUserInGroup(rootfs string) error {
	groupPath := filepath.Join(rootfs, "etc", "group")
	in, err := os.ReadFile(groupPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // No group file, nothing to do
		}
		return err
	}

	// Check whether a group named "user" or with GID 7575 already exists.
	// Group format: name:password:GID:member-list
	var lines []string
	var exists bool
	scanner := bufio.NewScanner(strings.NewReader(string(in)))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		if fields[0] == "user" {
			exists = true
		}
		if gid, err := strconv.ParseUint(fields[2], 10, 32); err == nil && uint32(gid) == ThundersnapGID {
			exists = true
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan group: %w", err)
	}

	if exists {
		return nil
	}

	newGroupEntry := fmt.Sprintf("user:x:%d:", ThundersnapGID)

	// Append the new group entry. Group ordering does not matter, so just add
	// it at the end (preserving the original trailing newline behavior).
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteString(newGroupEntry)
	out.WriteByte('\n')

	return atomicWriteFile(groupPath, []byte(out.String()), 0644)
}

// EnsureSudoers creates a sudoers.d drop-in file that grants passwordless
// sudo to the "user" account. This is necessary because containers typically
// need root access for package management, service control, etc.
//
// Returns nil if /etc/sudoers.d doesn't exist (container has no sudo).
func EnsureSudoers(rootfs string) error {
	sudoersDir := filepath.Join(rootfs, "etc", "sudoers.d")
	if _, err := os.Stat(sudoersDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // No sudo installed, skip
		}
		return err
	}

	// Create a drop-in file for the user account
	dropinPath := filepath.Join(sudoersDir, "thundersnap-user")
	content := "# Thundersnap: allow the user account passwordless sudo\nuser ALL=(ALL) NOPASSWD: ALL\n"

	// sudoers files must be mode 0440 and owned by root
	if err := os.WriteFile(dropinPath, []byte(content), 0440); err != nil {
		return fmt.Errorf("write sudoers drop-in: %w", err)
	}

	return nil
}

// passwdEntry represents a single line from /etc/passwd.
type passwdEntry struct {
	Username string
	Password string // usually "x" meaning shadow
	UID      uint32
	GID      uint32
	GECOS    string // comment/full name
	Home     string
	Shell    string
}

// parsePasswdEntry parses a single colon-delimited passwd line.
// Returns nil if the line is a comment, blank, or malformed.
func parsePasswdEntry(line string) *passwdEntry {
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	fields := strings.Split(line, ":")
	if len(fields) < 7 {
		return nil
	}
	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return nil
	}
	gid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return nil
	}
	return &passwdEntry{
		Username: fields[0],
		Password: fields[1],
		UID:      uint32(uid),
		GID:      uint32(gid),
		GECOS:    fields[4],
		Home:     fields[5],
		Shell:    fields[6],
	}
}

// UserInfo contains information about a user from /etc/passwd.
type UserInfo struct {
	Username string
	UID      uint32
	GID      uint32
	Home     string
	Shell    string
}

// LookupUser looks up a user by name in the container's /etc/passwd.
// Returns nil if the user is not found or the passwd file doesn't exist.
func LookupUser(rootfs, username string) *UserInfo {
	passwdPath := filepath.Join(rootfs, "etc", "passwd")
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return nil
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		entry := parsePasswdEntry(scanner.Text())
		if entry != nil && entry.Username == username {
			return &UserInfo{
				Username: entry.Username,
				UID:      entry.UID,
				GID:      entry.GID,
				Home:     entry.Home,
				Shell:    entry.Shell,
			}
		}
	}
	return nil
}

// ensureUserInShadow adds a "user" entry to /etc/shadow if the file exists
// and doesn't already have a "user" entry. The entry uses "!" (locked password)
// which allows login via su/sudo but not direct password auth.
func ensureUserInShadow(rootfs string) error {
	shadowPath := filepath.Join(rootfs, "etc", "shadow")
	in, err := os.ReadFile(shadowPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // No shadow file, nothing to do
		}
		return err
	}

	// Check if "user" already exists in shadow
	var lines []string
	var userExists bool
	scanner := bufio.NewScanner(strings.NewReader(string(in)))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if line != "" && !strings.HasPrefix(line, "#") {
			fields := strings.Split(line, ":")
			if len(fields) > 0 && fields[0] == "user" {
				userExists = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan shadow: %w", err)
	}

	if userExists {
		return nil
	}

	// Shadow format: username:password:lastchanged:min:max:warn:inactive:expire:reserved
	// "!" means locked (no password login, but su/sudo still work)
	// Use 0 for lastchanged (days since epoch) - doesn't matter for locked accounts
	newShadowEntry := "user:!:0:0:99999:7:::"

	// Insert after root line
	var out strings.Builder
	inserted := false
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
		if !inserted && line != "" && !strings.HasPrefix(line, "#") {
			fields := strings.Split(line, ":")
			if len(fields) > 0 && fields[0] == "root" {
				out.WriteString(newShadowEntry)
				out.WriteByte('\n')
				inserted = true
			}
		}
	}
	if !inserted {
		out.WriteString(newShadowEntry)
		out.WriteByte('\n')
	}

	// Shadow files are mode 0640 (or 0600)
	return atomicWriteFile(shadowPath, []byte(out.String()), 0640)
}

// atomicWriteFile writes data to path via a temp file + rename, preserving
// the existing file's mode if it exists; otherwise uses defaultMode.
func atomicWriteFile(path string, data []byte, defaultMode os.FileMode) error {
	mode := defaultMode
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup if rename didn't happen.
		os.Remove(tmpName)
	}()
	if _, err := io.Copy(tmp, strings.NewReader(string(data))); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
