#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
git -C "$repo_root" config core.hooksPath .githooks

cat <<'EOF'
Git hooks installed: core.hooksPath=.githooks

pre-commit (fast, seconds): goimports + gofmt on staged Go files.
pre-push (slower):          make preflight (formatting, module tidiness, vet,
                            staticcheck, short tests), and docs checks when
                            docs changed.

Coverage (minimum 70.0%, override with MIN_COVERAGE), race and gopls
diagnostics run in CI; `make quality` and `make gopls-check` reproduce them
locally. Run `make preflight` yourself to check without pushing.
EOF
