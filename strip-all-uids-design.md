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

## Current Implementation: Known Limitations

### When Stripping Happens

UID stripping (`StripRootfs`) runs only at **frame creation time** — when a snapshot is cloned into a live filesystem. It does NOT run:
- On frame restart (directory already exists → no-op)
- During runtime (no monitoring of `/etc/passwd`)
- At Docker import time (snapshots keep original UIDs)

### The Consistency Problem

This creates a window for inconsistent state:

1. User starts frame (stripping runs, all UIDs → 1000)
2. User runs `apt-get install postgresql` → creates UID 119, files owned by 119
3. Frame restarts → no stripping, UID 119 persists, **works fine**
4. User snapshots and creates new frame → stripping runs again
5. Now postgres is UID 1000, files chowned to 1000, **still works**

But this means:
- Behavior differs between "restart frame" and "snapshot/restore frame"
- Multiple services (postgres UID 119, redis UID 120) coexist during runtime, then collapse to shared UID 1000 on next frame creation
- Software with hardcoded numeric UID checks may work in original frame but break after snapshot/restore

### Why Not User Namespaces?

Linux user namespaces (`CLONE_NEWUSER`) only support 1:1 UID mappings — you cannot map "all UIDs except 0" to a single UID. The `uid_map`/`gid_map` format is:
```
<inside-ns-uid> <outside-ns-uid> <count>
```

Each range maps 1:1. You can have up to 5 lines, but they must be non-overlapping and cannot converge multiple UIDs to one.

However, you *could* map only two UIDs (0→0, 1000→1000) and rely on passwd rewriting so all name lookups return 1000. Then:
- `chown postgres:postgres` → passwd returns 1000 → works
- `chown 119:119` → **EINVAL** (unmapped UID)

This would fail loudly rather than silently accumulating divergent state.

### Why Not Run Everything as Root?

Running all processes as UID 0 would eliminate permission problems, but some software explicitly refuses:

- **PostgreSQL**: Hardcoded check — "root execution of the PostgreSQL server is not permitted"
- **Other daemons**: Many check `getuid() == 0` and exit for "security"

These checks exist in the application code, not the kernel, so capabilities like `CAP_DAC_OVERRIDE` don't help.

## Alternative Approaches Considered

### Option 1: Current Design (Implemented)
Strip at frame creation only. Accept inconsistency between runtime and post-snapshot state.

**Pros**: Simple, works for most cases
**Cons**: Silent behavioral changes across snapshot/restore cycles

### Option 2: User Namespaces + Pre-Stripping
Map only UIDs 0 and 1000. Strip at Docker import time (not just frame creation). Unmapped UIDs fail with EINVAL.

**Pros**: Fail-fast on invalid operations, consistent state
**Cons**: Breaks `apt-get install` for packages that create users with arbitrary UIDs unless `adduser` is also patched/wrapped

### Option 3: NSS Module
Ship a custom NSS module that makes `getpwnam("anything")` return UID 1000 (except root). Completely transparent to applications.

**Pros**: Most transparent fix, no passwd rewriting needed
**Cons**: Requires building/shipping OS-specific NSS modules

### Option 4: Wrap adduser/useradd
Intercept user creation commands to always use UID 1000.

**Pros**: Works with Option 2 to make package installation work
**Cons**: OS-specific, fragile, may miss edge cases

### Option 2+4 Combined
User namespaces (0+1000 only) plus wrapped `adduser`. Package installation would work because `adduser postgres` creates UID 1000, and `chown postgres:postgres` resolves to 1000 (mapped).

**Pros**: Consistent, fail-fast, package installation works
**Cons**: Requires OS-specific adduser wrappers; complexity

## Conclusion

For the target use case — AI coding agents running builds, tests, and single services — this simplified UID model works well. The vast majority of software uses name-based user resolution and doesn't care about UID uniqueness. Package managers, databases, web servers, and build tools all function correctly.

The model fails for multi-tenant isolation within a container, but that's explicitly not a goal. Each container is a single-user, single-purpose environment.

The current implementation has known consistency issues around snapshot/restore cycles. A more robust solution (Option 2+4) would enforce the two-UID model via user namespaces and intercept user creation, but requires OS-specific work. For now, we accept the current behavior and document the limitations.
