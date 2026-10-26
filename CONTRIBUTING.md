# Contributing to chord-gateway

Thanks for your interest in contributing.

## Development Prerequisites

- Go: use the version declared in `go.mod`
- `chord` binary available in your `PATH` (or set `chord_path` in config)
- Go quality tools:

```bash
go install golang.org/x/tools/cmd/goimports@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
```

## Local Setup

```bash
go mod download
go build ./...
```

Enable commit hooks once:

```bash
./scripts/setup-git-hooks.sh
```

This installs two hooks that split the quality gate by cost:

- `.githooks/pre-commit` runs `goimports` and `gofmt` on staged `.go` files. It deliberately stops there so `git commit`, `--amend`, and rebase steps stay interactive.
- `.githooks/pre-push` runs the checks that decide whether a branch is publishable: `scripts/check-go-quality.sh` (tests, coverage, `go test -race`, `go vet`, `staticcheck`), `scripts/check_gopls.sh`, and the documentation checks when docs changed.

Use the Makefile for the same commands without pushing:

```bash
make preflight     # what pre-push runs
make ci            # everything CI runs
make test-fast     # tests over a memory-backed TMPDIR when one is available
make help          # list every target
```

Current local/CI quality gate:

```bash
MIN_COVERAGE=70.0 ./scripts/check-go-quality.sh
```

This runs:

```bash
goimports -l -local github.com/keakon/chord-gateway .
go test -short -timeout=5m -coverprofile=coverage.out -coverpkg=./... ./...
go test -race -short -timeout=5m ./...
go tool cover -func=coverage.out
# CI requires total coverage >= 70.0%.
go vet ./...
staticcheck -checks 'all,-ST1000' ./...
```

Keep the flags in sync with `scripts/check-go-quality.sh` and `.github/workflows/ci.yml`: `-coverpkg=./...` and the staticcheck check set both change what the gate measures, and a copy that drifts quietly runs less than CI does.

Faster local test runs are available when a memory-backed filesystem is present. `scripts/test_tmpdir.sh` resolves one; the first usable candidate wins:

1. `CHORD_GATEWAY_TEST_TMPDIR`
2. `CHORD_TEST_TMPDIR` (chord's variable, so one export covers both checkouts)
3. `.chord/test-tmpdir`, an untracked repo-local file whose first non-comment line is the path
4. the platform default: `/dev/shm` on Linux, or the APFS RAM disk `ChordTestRam` on macOS, shared with the chord checkout so both reuse one disk

`make test-fast` points `TMPDIR` at it, and `scripts/check-go-quality.sh` — the coverage/race gate behind `git push`, `make quality`, `make preflight`, and `make ci` — resolves the same directory for its test runs. Both fall back to the default temp dir when no candidate is usable. The CI workflow deliberately stays on the default filesystem: it calls neither script and should keep exercising the realistic worst case. For other entry points, export the variables yourself, for example `TMPDIR=$(./scripts/test_tmpdir.sh) make test-cover`; Go reads `TMP`/`TEMP` instead of `TMPDIR` on Windows, so the helper scripts set all three. The speed-up comes from the persistence tests' fsyncs; the cross-process contract tests are unaffected. `make clean` detaches the shared RAM disk, so neither checkout can reuse it until one recreates it.

Run the gate before opening a PR:

```bash
make ci
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
