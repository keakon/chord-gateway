# Contributing to chord-gateway

Thanks for your interest in contributing.

## Development Prerequisites

- Go: use the version declared in `go.mod`
- `chord` binary available in your `PATH` (or set `chord_path` in config)

## Local Setup

```bash
go mod download
go build ./...
```

Enable commit hooks once:

```bash
./scripts/setup-git-hooks.sh
```

This installs `.githooks/pre-commit`, which runs `gofmt` on staged `.go` files and then executes the shared Go quality checks before each commit.

Current local/CI quality gate:

```bash
MIN_COVERAGE=60.0 ./scripts/check-go-quality.sh
```

This runs:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
# CI requires total coverage >= 60.0%.
go vet ./...
staticcheck -checks 'all,-ST*' ./...
```

Run tests before opening a PR:

```bash
./scripts/check-go-quality.sh
```

## Pull Request Guidelines

- Keep changes focused and avoid unrelated refactors.
- Add or update tests for behavior changes.
- If user-facing behavior/config changes, update docs.
- For documentation updates, keep English and Chinese docs in sync:
  - `README.md` / `README_CN.md`
  - `QUICKSTART.md` / `QUICKSTART_CN.md`
  - `CHANGELOG.md` / `CHANGELOG_CN.md`
  - `docs/index.md` / `docs/index_CN.md`
  - `docs/configuration.md` / `docs/configuration_CN.md`
  - `docs/usage.md` / `docs/usage_CN.md`
  - `docs/operations.md` / `docs/operations_CN.md`
  - `docs/permissions-and-safety.md` / `docs/permissions-and-safety_CN.md`
  - `docs/troubleshooting.md` / `docs/troubleshooting_CN.md`
  - `docs/event-visibility.md` / `docs/event-visibility_CN.md`

## Commit Scope

Typical acceptable scope for one PR:

- one bug fix
- one small feature
- one documentation improvement set

## Security Reporting

Do not report vulnerabilities in public issues. See [SECURITY.md](./SECURITY.md).
