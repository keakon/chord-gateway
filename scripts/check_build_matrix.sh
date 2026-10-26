#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# README's support scope documents exactly this set; release.yml builds the same
# targets for tagged releases. Keeping one list here means a target cannot be
# dropped from the local gate and CI independently.
#
# This complements check_gopls.sh rather than duplicating it: that gate selects
# the files the *current* platform compiles, so a windows-only or unix-only twin
# gets no analysis at all on the other platforms. A cross build is the cheapest
# way to type-check those files without a runner per OS.
targets=(
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
  windows/amd64
)

for target in "${targets[@]}"; do
  if ! GOOS="${target%/*}" GOARCH="${target#*/}" go build -o /dev/null ./... 2>/tmp/build-matrix.err; then
    echo "cross build failed: ${target}" >&2
    cat /tmp/build-matrix.err >&2
    exit 1
  fi
  echo "build ok: ${target}"
done
echo "cross build: passed on ${#targets[@]} target(s)"
