#!/usr/bin/env bash
# Resolve a memory-backed temp directory for local test runs.
#
# The Go test suite is dominated by durable writes: persistence here fsyncs every
# write, depends on synchronous store pages, and exercises restart recovery by
# killing processes mid-write. On a real disk each fsync costs milliseconds and
# parallelism cannot hide it. Pointing TMPDIR at a memory-backed filesystem keeps
# every code path and durability guarantee intact while removing that latency.
#
# Resolution order (first writable directory wins):
#   1. $CHORD_GATEWAY_TEST_TMPDIR
#   2. .chord/test-tmpdir (repo-local, untracked) — first non-empty, non-comment line
#   3. platform default: /dev/shm on Linux; a reused-or-created APFS RAM disk on macOS
#
# Prints the chosen directory, or nothing when no memory-backed filesystem is
# available, so callers fall back to the default temp dir. Windows has no
# built-in memory filesystem and Go ignores TMPDIR there; point TMP or TEMP at a
# RAM disk or fast SSD instead.
#
# Usage:
#   scripts/test_tmpdir.sh            # print the directory to use
#   scripts/test_tmpdir.sh --destroy  # tear down the macOS RAM disk this may create

set -euo pipefail

volume_name="GatewayTestRam"
# 4 GiB expressed in 512-byte sectors, the unit `ram://` expects. Sized for a
# cold Go build cache, where the build work dir holds every compiled package
# archive and can exceed 1 GiB. The disk is sparse, so only written data is
# backed by memory.
size_sectors=$((4 * 1024 * 1024 * 1024 / 512))
mount_point="/Volumes/${volume_name}"

# usable reports whether dir can back TMPDIR for go test: it must exist, be
# writable, and allow executing files, because Go builds the test binary inside
# TMPDIR and execs it. A noexec mount (common for /dev/shm inside containers)
# is writable but fails there with "fork/exec ...: permission denied".
usable() {
	local dir="${1:-}"
	[[ -n "$dir" && -d "$dir" && -w "$dir" ]] || return 1
	local probe ok=1
	probe="$(mktemp "${dir%/}/cgw-exec-probe.XXXXXX" 2>/dev/null)" || return 1
	if (set +e; printf '#!/bin/sh\nexit 0\n' >"${probe}" && chmod +x "${probe}" && "${probe}") >/dev/null 2>&1; then
		ok=0
	fi
	rm -f "${probe}"
	return "${ok}"
}

case "${1:-}" in
	--destroy)
		if [[ -d "${mount_point}" ]]; then
			diskutil eject "${mount_point}" >/dev/null 2>&1 || true
		fi
		exit 0
		;;
	"") ;;
	*)
		echo "usage: $0 [--destroy]" >&2
		exit 2
		;;
esac

# 1. Explicit environment override.
if [[ -n "${CHORD_GATEWAY_TEST_TMPDIR:-}" ]]; then
	if usable "${CHORD_GATEWAY_TEST_TMPDIR}"; then
		echo "${CHORD_GATEWAY_TEST_TMPDIR}"
		exit 0
	fi
	echo "warning: CHORD_GATEWAY_TEST_TMPDIR=${CHORD_GATEWAY_TEST_TMPDIR} is not a writable directory; ignoring" >&2
fi

# 2. Repo-local config file. .chord/ holds local-only content and is ignored;
# a developer who wants a fixed location writes it there with a GUI-less editor
# instead of exporting it per shell.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
conf="${repo_root}/.chord/test-tmpdir"
if [[ -f "${conf}" ]]; then
	configured="$(grep -vE '^[[:space:]]*(#|$)' "${conf}" | head -n1 | tr -d '[:space:]' || true)"
	if [[ -n "${configured}" ]]; then
		if usable "${configured}"; then
			echo "${configured}"
			exit 0
		fi
		echo "warning: ${conf} points at ${configured}, which is not a writable directory; ignoring" >&2
	fi
fi

# 3. Platform default.
case "$(uname -s)" in
	Linux)
		if usable /dev/shm; then
			echo /dev/shm
			exit 0
		fi
		if [[ -e /dev/shm ]]; then
			echo "warning: /dev/shm is not usable as a test temp dir (must be writable and allow exec); falling back to the default temp dir" >&2
		fi
		;;
	Darwin)
		if usable "${mount_point}"; then
			echo "${mount_point}"
			exit 0
		fi
		dev="$(hdiutil attach -nomount "ram://${size_sectors}" 2>/dev/null | awk '{print $1; exit}' || true)"
		if [[ -n "${dev}" ]] && diskutil erasevolume APFS "${volume_name}" "${dev}" >/dev/null 2>&1 && usable "${mount_point}"; then
			echo "${mount_point}"
			exit 0
		fi
		echo "warning: could not create an APFS RAM disk; falling back to the default temp dir" >&2
		;;
esac

# No memory-backed filesystem available: emit nothing so the caller keeps the default.
exit 0
