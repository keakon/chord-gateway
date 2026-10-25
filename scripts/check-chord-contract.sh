#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# Explicit selection makes a missing/unbuildable companion fail, never skip.
export CHORD_GATEWAY_CONTRACT_CHORD_REPO="${CHORD_GATEWAY_CONTRACT_CHORD_REPO:-$repo_root/../chord}"
if [[ ! -f "$CHORD_GATEWAY_CONTRACT_CHORD_REPO/go.mod" ]]; then
  echo "Chord checkout required: $CHORD_GATEWAY_CONTRACT_CHORD_REPO" >&2
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
