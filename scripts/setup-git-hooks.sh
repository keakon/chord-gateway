#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
git -C "$repo_root" config core.hooksPath .githooks

cat <<'EOF'
Git hooks installed: core.hooksPath=.githooks

pre-commit (fast, seconds): goimports + gofmt on staged Go files.
pre-push (slow, minutes):   goimports check, scripts/check-go-quality.sh
                            (tests + coverage + race + vet + staticcheck),
                            scripts/check_gopls.sh, and docs checks when docs
                            changed.

Correctness gates live at pre-push so `git commit` and history rewrites stay
interactive. Run `make preflight` yourself to check without pushing.
Default minimum coverage: 70.0% (override with MIN_COVERAGE if needed)
EOF
