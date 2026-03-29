# Simplified UID Model for Thundersnap Containers

This document analyzes a simplified permission model where containers have only two effective UIDs: root (0) and a single non-root UID for everything else.

## The Model

- **Root (UID 0)**: Works normally inside the container
- **All non-root UIDs**: Map to a single host UID
- `/etc/passwd` is modified so all non-root user entries (postgres, www-data, mysql, etc.) resolve to the same UID
- setuid-root binaries work (we support both root and user UIDs)
- Single user per container, typically single service, no intra-container isolation required

## Why This Works

Most Unix programs resolve usernames via `getpwnam()` / `getgrnam()`, then operate on the returned UID. They don't care if multiple names map to the same UID. The typical pattern:

```c
struct passwd *pw = getpwnam("postgres");
setuid(pw->pw_uid);  // Works fine with UID 1000
```

As long as:
1. The username exists in `/etc/passwd`
2. The UID is valid
3. Files are owned by that UID

...the program is satisfied.

## Package Installation: Works

### dpkg/apt

postinst scripts typically:
```bash
adduser --system postgres
chown -R postgres:postgres /var/lib/postgresql
```

With the hacked `/etc/passwd`:
- `adduser` succeeds (creates entry with the shared UID)
- `chown postgres:postgres` succeeds (valid UID from passwd)
- Service starts, resolves its username, gets the shared UID, works

### rpm

`%attr(-, postgres, postgres)` resolves via NSS to the shared UID. Files get correct ownership from the kernel's perspective.

### Nix

Single-user mode works fine. Multi-user mode (which requires distinct UIDs for build isolation) isn't needed since we don't require intra-container isolation.

## Services and Daemons: Work

### Databases (PostgreSQL, MySQL, MongoDB, Redis)

Ownership checks are typically:
```c
stat(DataDir, &st);
if (st.st_uid != geteuid())
    fatal("wrong ownership");
```

If the postgres user has UID 1000, PostgreSQL runs as UID 1000, and the data directory is owned by UID 1000 — the check passes.

### Privilege-dropping services (nginx, Apache)

Start as root, bind privileged ports, then `setuid(www-data)`. As long as `www-data` resolves to a valid UID, this works.

### setuid binaries (sudo, su, passwd)

Owned by root with setuid bit. User executes them, they elevate to root, perform their function. Works normally.

### systemd DynamicUser

Works, though all dynamic users share one UID. Fine for single-service containers.

## Package Building: Works

### Debian packages

`dpkg-deb` uses `--root-owner-group` by default, forcing everything to UID/GID 0 in the archive. The built `.deb` is identical to one built on a normal system.

Edge case: If `debian/rules` explicitly `chown`s files to a service user before packaging, the numeric UID (your shared UID) gets embedded. Unusual, but possible.

### Nix packages

Nix is designed for reproducibility across different machines with different UIDs. It normalizes all store paths to root ownership. The collapsed UID model doesn't affect build outputs.

### RPM packages

The `.spec` file controls ownership via `%attr()` directives, not filesystem ownership during build. Built RPMs are correct.

### Plain tarballs

**Caveat**: `tar cvf` without `--owner=root --group=root` embeds the actual filesystem UID. Use explicit ownership flags when creating distribution archives.

## Edge Cases That Don't Work

### Programs with hardcoded numeric UID checks

Rare configs like:
```
allowed_uid = 33
```

Will break if your passwd maps that user to UID 1000 instead of 33.

### Software that validates passwd consistency

Paranoid software checking for duplicate UIDs:
```c
if (alice->pw_uid == bob->pw_uid) {
    // refuse to run
}
```

Very rare in practice.

### Nested containers or user namespaces

Docker-in-Docker, Podman, Nix multi-user builds — anything requiring UID namespace isolation won't work. Not relevant for the single-service model.

### UID-based lockfile protocols

Some lock mechanisms encode owning UID. Multiple "users" (same UID) acquiring locks may have weird semantics. Rarely relevant.

### Audit trails

`auditd` and UID-tracking logs show all activity as one UID. Forensics are harder, but functionality is unaffected.

## Recommendations

1. **Pre-bake service users into base images**: Run `adduser` for common services during snapshot creation so postinst scripts find existing users.

2. **Keep setuid binaries**: Unlike some container hardening guides, we want `sudo`/`su` to work since we support root.

3. **Avoid package installation at runtime if possible**: Pre-install in base snapshots. Runtime installs work but are slower and may have edge cases.

4. **Use explicit ownership in tarballs**: When building distribution archives, always use `--owner=root --group=root`.

5. **Document the model**: Users should understand that intra-container user isolation doesn't exist.

## Conclusion

For the target use case — AI coding agents running builds, tests, and single services — this simplified UID model works well. The vast majority of software uses name-based user resolution and doesn't care about UID uniqueness. Package managers, databases, web servers, and build tools all function correctly.

The model fails for multi-tenant isolation within a container, but that's explicitly not a goal. Each container is a single-user, single-purpose environment.
