#!/bin/sh
# Compute the thundersnap build version string to embed in binaries via
# -ldflags "-X github.com/tailscale/thundersnap/version.Version=<output>".
#
# The goal is the output of `git describe --tags --always --dirty`, which is the
# single, well-defined "what version is this tree" string used across thundersnap
# (package names, embedded --version output, client/server version checks). The
# git repository is not available at *runtime* (binaries run inside containers
# and VMs with no checkout), so this runs at *build* time and bakes the result in.
#
# A single leading "v" is stripped from the final string (see printver below) so
# that a tag like "v0.03" yields the version string "0.03", matching the Debian
# convention that package versions begin with a digit and matching the v-less
# form used in .deb/.rpm/.tgz filenames. This keeps `ts --version`, `ts version`,
# the package filename, and dpkg's Version field all reporting the same string.
#
# Resolution order:
#   1. $THUNDERSNAP_VERSION   explicit override (CI, release tarballs)
#   2. `git describe`          plain git repos AND jj repos colocated with git
#                              (the normal thundersnap dev layout)
#   3. jj                     when the `git` binary is absent but jj is present
#                             (a jj-only checkout), a best-effort describe-style
#                             string: <tag>-<N>-g<short>[-dirty] or g<short>
#   4. ./VERSION file          baked for release source tarballs (no VCS)
#   5. "unknown"                last resort
#
# This script is intentionally POSIX sh (no bashisms) so it runs anywhere the
# build does. It never exits non-zero: a build must always get a version string.
set -u

# printver prints its argument with a single leading "v" removed, so a tag
# "v0.03" produces the version string "0.03". This normalizes every resolution
# path below to the same v-less form used in package filenames and dpkg's
# Version field. It is a no-op for strings that don't start with "v".
printver() {
	v="$1"
	printf '%s\n' "${v#v}"
}

# 1. Explicit override.
if [ -n "${THUNDERSNAP_VERSION:-}" ]; then
	printver "$THUNDERSNAP_VERSION"
	exit 0
fi

# 2. git describe. This works for plain git checkouts and for jj repos that are
#    colocated with git (jj writes a real .git, and git describe sees the tags).
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
	v=$(git describe --tags --always --dirty 2>/dev/null || true)
	if [ -n "$v" ]; then
		printver "$v"
		exit 0
	fi
fi

# 3. jj-only fallback: approximate `git describe` when the `git` binary (or a
#    .git directory) is unavailable but jj is. The output is
#    "<tag>-<N>-g<short>" where <tag> is the nearest reachable tag, <N> is the
#    number of commits in tag..@ (mirroring git describe's count), and <short>
#    is the first 7 hex of @'s commit id; "-dirty" is appended when the working
#    copy differs from @. With no reachable tag, "<short>" alone is printed
#    (the --always form). For exact git-describe parity in colocated repos, the
#    git path above is used; this jj path is for pure-jj checkouts.
if command -v jj >/dev/null 2>&1 && jj root >/dev/null 2>&1; then
	cur=$(jj log -r '@' --no-graph -T 'commit_id ++ "\n"' 2>/dev/null | head -n1 | tr -d '\n\r')
	if [ -n "$cur" ]; then
		short=$(printf '%s' "$cur" | cut -c1-7)
		# Nearest tag reachable from @: "latest(tags() & ::@)".
		tag=$(jj log -r 'latest(tags() & ::@)' --no-graph -T 'tags ++ "\n"' 2>/dev/null | head -n1 | tr -d '\n\r')
		tagcommit=$(jj log -r 'latest(tags() & ::@)' --no-graph -T 'commit_id ++ "\n"' 2>/dev/null | head -n1 | tr -d '\n\r')
		if [ -n "$tag" ] && [ "$tagcommit" = "$cur" ]; then
			# @ is exactly a tagged commit.
			out="$tag"
		elif [ -n "$tag" ] && [ -n "$tagcommit" ]; then
			# Commits reachable from @ but not from the tag (git's tag..HEAD),
			# one per line so grep -c . counts commits, not a glued blob.
			n=$(jj log -r "$tagcommit..@" --no-graph -T 'commit_id ++ "\n"' 2>/dev/null | grep -c .)
			out=$(printf '%s-%s-g%s' "$tag" "$n" "$short")
		else
			# No reachable tag (--always form: bare short hash, no "g" prefix,
			# to match `git describe --always`).
			out="$short"
		fi
		# Dirty if the working copy differs from the recorded @ tree.
		if [ -n "$(jj diff --summary 2>/dev/null)" ]; then
			out="$out-dirty"
		fi
		printver "$out"
		exit 0
	fi
fi

# 4. Baked VERSION file (release source tarballs ship this).
if [ -f ./VERSION ]; then
	v=$(cat ./VERSION 2>/dev/null)
	if [ -n "$v" ]; then
		printver "$v"
		exit 0
	fi
fi

# 5. Last resort.
printf 'unknown\n'
