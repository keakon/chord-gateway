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

该脚本安装两个钩子，按开销分层：

- `.githooks/pre-commit` 对已暂存的 `.go` 文件执行 `goimports` 和 `gofmt`。它有意到此为止，让 `git commit`、`--amend` 和 rebase 步骤保持交互式速度。
- `.githooks/pre-push` 运行 `make preflight`：格式化、模块整洁性、`go vet`、`staticcheck` 和带 `-short` 的测试，文档有改动时再加文档检查。`-short` 会跳过跨仓契约测试，否则它们会构建同级 `../chord` 工作树；需要时用 `make contract` 运行。覆盖率下限、`go test -race` 和 gopls 诊断交给 CI 的 `test`、`race`、`gopls` job，每个推送的提交都会跑。

不想推送就执行同样命令时用 Makefile：

```bash
make preflight     # pre-push 的内容
make ci            # CI 的全部内容
make test-fast     # 在可用时把 TMPDIR 指向内存文件系统后跑测试
make help          # 列出所有 target
```

完整质量门禁（即 CI 的 `test` 与 `race` job 执行的内容；本地用 `make quality` 或 `make test-cover`）：

```bash
MIN_COVERAGE=70.0 ./scripts/check-go-quality.sh
```

该脚本会执行：

```bash
goimports -l -local github.com/keakon/chord-gateway .
go test -short -timeout=5m -coverprofile=coverage.out -coverpkg=./... ./...
go test -race -short -timeout=5m ./...
./scripts/check-coverage.sh coverage.out
# CI 要求总覆盖率 >= 70.0%。
go vet ./...
staticcheck -checks 'all,-ST1000' ./...
```

请让这些参数与 `scripts/check-go-quality.sh`、`.github/workflows/ci.yml` 保持一致：`-coverpkg=./...` 与 staticcheck 的检查集都会改变门禁的度量口径，抄错一份就会比 CI 跑得更少。

存在内存文件系统时本地测试可以更快。`scripts/test_tmpdir.sh` 负责解析一个目录，按以下顺序取第一个可用项：

1. `CHORD_GATEWAY_TEST_TMPDIR`
2. `CHORD_TEST_TMPDIR`（chord 使用的变量，导出一次即可同时覆盖两个仓库）
3. `.chord/test-tmpdir`（未跟踪的仓库本地文件，取第一个非注释行作为路径）
4. 平台默认：Linux 用 `/dev/shm`；macOS 用 APFS RAM disk `ChordTestRam`，与 chord 仓库共享，避免重复创建内存盘

`make test-fast` 会把 `TMPDIR` 指向它；`scripts/check-go-quality.sh`（`make quality`、`make test-cover`、`make ci` 背后的覆盖率 / race 门禁）也会用同一个目录跑测试。没有可用目录时两者都回退到默认临时目录。CI workflow 有意保持默认文件系统：它不调用这两个脚本，需要继续覆盖更接近真实的最坏情况。其他入口可以自行导出，例如 `TMPDIR=$(./scripts/test_tmpdir.sh) make test-cover`；Windows 上 Go 读的是 `TMP`/`TEMP` 而不是 `TMPDIR`，所以辅助脚本会同时设置三者。加速主要来自持久化测试的 fsync；跨进程契约测试不受影响。只要 `ChordTestRam` 可用就直接复用，没有 owner 记录也一样；记录只用于授权清理。owner 记录保存挂载卷的设备节点、卷 UUID、父磁盘和 attach 得到的 physical store。`make clean` 仅在当前挂载与前三个字段匹配、且记录的 physical store 仍支撑该父磁盘时，才弹出这个 physical store；无法验证归属的挂载保持不动。刚 attach 的盘无法确认就是 `/Volumes/ChordTestRam` 上的那块时，辅助脚本不会弹出或重命名这个挂载（其他进程可能正在用），而是改用一块私有盘 `/Volumes/ChordTestRam-<pid>-<random>`。`make test-fast` 和 `scripts/check-go-quality.sh` 会在运行结束时弹出这块私有盘；其他调用方用 `scripts/test_tmpdir.sh --release <dir>` 释放，它不会弹出共享挂载。只是 owner 记录写不进去时，照常使用共享挂载，`make clean` 会拒绝清理它，警告里会给出 `diskutil eject` 命令。

提交 PR 前请执行门禁：

```bash
make ci
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
