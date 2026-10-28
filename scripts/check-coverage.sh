#!/usr/bin/env bash
set -euo pipefail

# Report statement coverage from a `-coverprofile` file, grouped by package.
#
# -coverpkg=./... makes every test binary emit the same blocks, so a block is
# counted once and keeps its highest count across binaries; that matches what
# `go tool cover` reports. The per-function table it would print adds tens of
# thousands of lines to the job log for the same total, so the table here stays
# at package granularity.
#
# Usage: check-coverage.sh [profile]   (COVERAGE_FILE overrides the default path)

coverage_file="${1:-${COVERAGE_FILE:-coverage.out}}"
min_coverage="${MIN_COVERAGE:-70.0}"

if [[ ! -s "${coverage_file}" ]]; then
  echo "coverage profile not found: ${coverage_file}" >&2
  exit 1
fi

coverage_report="$(awk '
  NR == 1 { next }
  {
    key = $1
    if (key in stmts) {
      if ($3 + 0 > best[key]) best[key] = $3 + 0
      next
    }
    file = $1
    sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
    pkg = file
    sub(/[\/\\][^\/\\]+$/, "", pkg)
    stmts[key] = $2
    pkgs[key] = pkg
    best[key] = $3 + 0
  }
  END {
    for (key in stmts) {
      pkgStmts[pkgs[key]] += stmts[key]
      all += stmts[key]
      if (best[key] > 0) {
        pkgCovered[pkgs[key]] += stmts[key]
        allCovered += stmts[key]
      }
    }
    if (all > 0) printf "total %.1f\n", 100 * allCovered / all
    for (pkg in pkgStmts) printf "%.1f %s\n", 100 * pkgCovered[pkg] / pkgStmts[pkg], pkg
  }' "${coverage_file}")"

total_coverage="$(printf '%s\n' "${coverage_report}" | awk '$1 == "total" { print $2 }')"
if [[ -z "${total_coverage}" ]]; then
  echo "failed to parse total coverage from ${coverage_file}" >&2
  exit 1
fi

package_coverage="$(printf '%s\n' "${coverage_report}" | grep -v '^total ' | sort -n | awk '{ printf "%6.1f%%  %s\n", $1, $2 }')"

if awk -v total="${total_coverage}" -v min="${min_coverage}" 'BEGIN { exit !(total + 0 >= min + 0) }'; then
  echo "per-package coverage (lowest first):"
  printf '%s\n' "${package_coverage}"
  printf 'coverage check passed: total %s%% >= required %s%%\n' "${total_coverage}" "${min_coverage}"
else
  echo "per-package coverage (lowest first):" >&2
  printf '%s\n' "${package_coverage}" >&2
  echo "coverage check failed: total ${total_coverage}% < required ${min_coverage}%" >&2
  exit 1
fi
