#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# Explicit selection makes a missing/unbuildable companion fail, never skip.
# The tests open this path from their own package directory, so a relative
# selection such as the Makefile's `CHORD_REPO=../chord` must be resolved
# against the repository root here; otherwise it points inside internal/.
chord_repo="${CHORD_GATEWAY_CONTRACT_CHORD_REPO:-$repo_root/../chord}"
case "$chord_repo" in
  /*) ;;
  *) chord_repo="$repo_root/$chord_repo" ;;
esac
chord_repo="$(cd "$chord_repo" 2>/dev/null && pwd || printf '%s' "$chord_repo")"
export CHORD_GATEWAY_CONTRACT_CHORD_REPO="$chord_repo"
if [[ ! -f "$chord_repo/go.mod" ]]; then
  echo "Chord checkout required: $chord_repo" >&2
  exit 1
fi
# -v output is inspected below because `go test` exits 0 when -run matches
# nothing; without this check a renamed test would silently disable the gate.
test_output="$(go test -count=1 -v -timeout="${GO_TEST_TIMEOUT:-5m}" -run '^TestChord.*Contract' ./internal/gateway 2>&1)" || {
  status=$?
  printf '%s\n' "$test_output"
  exit "$status"
}
printf '%s\n' "$test_output"
if ! grep -q '^=== RUN   TestChord' <<<"$test_output"; then
  echo "contract gate matched no tests; check the -run pattern" >&2
  exit 1
fi
