#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
git -C "$repo_root" config core.hooksPath .githooks

cat <<'EOF'
Git hooks installed: core.hooksPath=.githooks
pre-commit will:
  1. run goimports + gofmt on staged .go files
  2. run scripts/check-go-quality.sh
Default minimum coverage: 60.0% (override with MIN_COVERAGE if needed)
EOF
