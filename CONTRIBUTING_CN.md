# 参与贡献（chord-gateway）

感谢你为 chord-gateway 做贡献。

## 开发前提

- Go 版本以 `go.mod` 为准
- 本机可用 `chord` 可执行文件（或在配置中设置 `chord_path`）
- Go 质量检查工具：

```bash
go install golang.org/x/tools/cmd/goimports@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
```

## 本地开发

```bash
go mod download
go build ./...
```

首次请启用提交 hooks：

```bash
./scripts/setup-git-hooks.sh
```

该脚本会安装 `.githooks/pre-commit`，在每次提交前先对已暂存的 `.go` 文件执行 `goimports` 和 `gofmt`，再运行共享的 Go 质量检查。

当前本地 / CI 质量门禁：

```bash
MIN_COVERAGE=70.0 ./scripts/check-go-quality.sh
```

该脚本会执行：

```bash
goimports -l -local github.com/keakon/chord-gateway .
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
# CI 要求总覆盖率 >= 70.0%。
go vet ./...
staticcheck -checks 'all,-ST*' ./...
```

提交 PR 前请执行：

```bash
./scripts/check-go-quality.sh
```

## PR 规范

- 变更尽量聚焦，避免夹带无关重构。
- 行为变更需要补测试或更新现有测试。
- 用户可见行为或配置变更必须同步文档。
- 文档更新请保持中英文一致：
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

## 提交范围建议

一个 PR 建议只包含以下之一：

- 一个 bug 修复
- 一个小功能
- 一组文档完善

## 安全问题反馈

请不要在公开 issue 中提交漏洞细节。参见 [SECURITY.md](./SECURITY.md)。
