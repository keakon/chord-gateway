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
go test -count=1 -timeout="${GO_TEST_TIMEOUT:-5m}" -run '^TestChord.*Contract' .
