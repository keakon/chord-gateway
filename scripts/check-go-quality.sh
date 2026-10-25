#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

coverage_file="${COVERAGE_FILE:-coverage.out}"
min_coverage="${MIN_COVERAGE:-70.0}"
test_timeout="${GO_TEST_TIMEOUT:-5m}"

if ! command -v go >/dev/null 2>&1; then
  echo "go not found in PATH" >&2
  exit 1
fi

if ! command -v staticcheck >/dev/null 2>&1; then
  echo "staticcheck not found in PATH" >&2
  exit 1
fi

if ! command -v goimports >/dev/null 2>&1; then
  echo "goimports not found in PATH" >&2
  echo "install with: go install golang.org/x/tools/cmd/goimports@latest" >&2
  exit 1
fi

echo "==> goimports -l -local github.com/keakon/chord-gateway ."
goimports_diff="$(goimports -l -local github.com/keakon/chord-gateway . 2>/dev/null)"
if [[ -n "$goimports_diff" ]]; then
  echo "goimports check failed: the following files need formatting:" >&2
  printf '%s\n' "$goimports_diff" >&2
  echo "run: goimports -w -local github.com/keakon/chord-gateway ." >&2
  exit 1
fi

echo "==> go test -short -timeout=${test_timeout} -coverprofile=${coverage_file} -coverpkg=./... ./..."
go test -short -timeout="$test_timeout" -coverprofile="$coverage_file" -coverpkg=./... ./...

echo "==> go test -race -short -timeout=${test_timeout} ./..."
go test -race -short -timeout="$test_timeout" ./...

echo "==> go tool cover -func=${coverage_file}"
cover_output="$(go tool cover -func="$coverage_file")"
printf '%s
' "$cover_output"

total_coverage="$(printf '%s
' "$cover_output" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
if [[ -z "$total_coverage" ]]; then
  echo "failed to parse total coverage" >&2
  exit 1
fi

awk -v total="$total_coverage" -v min="$min_coverage" 'BEGIN {
  if (total + 0 < min + 0) {
    printf("coverage check failed: total %.1f%% < required %.1f%%\n", total + 0, min + 0)
    exit 1
  }
  printf("coverage check passed: total %.1f%% >= required %.1f%%\n", total + 0, min + 0)
}'

echo "==> go vet ./..."
go vet ./...

echo "==> staticcheck -checks 'all,-ST1000' ./..."
staticcheck -checks 'all,-ST1000' ./...

echo "==> cross-repo contract gate"
contract_repo="${CHORD_GATEWAY_CONTRACT_CHORD_REPO:-../chord}"
if [[ -f "$contract_repo/go.mod" ]]; then
  echo "Chord checkout found at $contract_repo; run ./scripts/check-chord-contract.sh to enforce the cross-repo contract tests."
else
  echo "No Chord checkout at $contract_repo; the cross-repo contract tests are not covered here. Run ./scripts/check-chord-contract.sh with CHORD_GATEWAY_CONTRACT_CHORD_REPO set once a checkout is available."
fi
