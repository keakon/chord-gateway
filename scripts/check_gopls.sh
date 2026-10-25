#!/usr/bin/env bash
# Run gopls diagnostics over exactly the Go files Go selects for the target
# platform.
#
# Feeding gopls every tracked .go file is a correctness bug rather than a style
# issue: this repository ships build-tagged files such as
# config_persistence_windows.go and config_persistence_unix.go, and a gopls
# instance configured for linux will resolve the wrong twin, reporting
# diagnostics against code that does not compile on this platform while skipping
# the file that does. `go list -json` is the only source that already applied the
# GOOS/GOARCH file selection.
set -euo pipefail

go_command="${CG_GO:-go}"
gopls_command="${CG_GOPLS:-gopls}"

scratch=$(mktemp -d "${TMPDIR:-/tmp}/chord-gateway-gopls.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
if (( $# == 0 )); then
  set -- ./...
fi

# Do not use -e for the load: a partially failing `go list` must surface as a
# failed gate instead of quietly producing a short (and passing) file list.
if ! "$go_command" list -json "$@" > "$scratch/packages.json"; then
  echo "check_gopls: go list failed; refusing to report a clean result" >&2
  exit 1
fi

python3 - "$scratch/packages.json" > "$scratch/files" <<'PY'
import json
import os
import sys

text = open(sys.argv[1], encoding="utf-8").read()
decoder = json.JSONDecoder()
position = 0
files = set()
while position < len(text):
    if text[position].isspace():
        position += 1
        continue
    package, position = decoder.raw_decode(text, position)
    for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
        files.update(os.path.join(package["Dir"], name) for name in package.get(field, []))
for path in sorted(files):
    sys.stdout.buffer.write(os.fsencode(path) + b"\0")
PY

if [[ ! -s "$scratch/files" ]]; then
  echo "check_gopls: no Go files selected; refusing to report a clean result" >&2
  exit 1
fi

# gopls check can print errors and still exit zero, so a clean run requires both
# a zero exit code and no output. Do not filter platform-tagged messages: those
# are precisely the diagnostics this platform-aware file selection exists to get
# right.
if ! xargs -0 "$gopls_command" check < "$scratch/files" > "$scratch/diagnostics" 2>&1; then
  cat "$scratch/diagnostics" >&2
  echo "check_gopls: gopls check failed" >&2
  exit 1
fi
if [[ -s "$scratch/diagnostics" ]]; then
  cat "$scratch/diagnostics" >&2
  echo "check_gopls: gopls diagnostics found" >&2
  exit 1
fi

count=$(tr -cd '\0' < "$scratch/files" | tr '\0' 'x' | wc -c | tr -d ' ')
echo "check_gopls: passed on ${count} platform-selected Go file(s)"
