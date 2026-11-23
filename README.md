# GoAgent Harness

基于 [GoAgent](https://github.com/kart-io/goagent) 的可配置 Agent 装配与运行组件库。面向需要在 CLI、IM 助手或 HTTP 服务中嵌入工程 Agent 的 Go 开发者。

GoAgent 提供模型与工具循环、类型化工作流、持久恢复、预算、观测和执行环境接口；本项目提供 Preset 装配、编码工具、计划模式、技能、子 Agent 配置、Claude Code 命令钩子与上下文压缩。两个项目单向依赖，不复制执行引擎。

基于 Hermes 全文拆解的[增量设计提案](docs/design/HERMES_ADOPTION.md)明确技能 Catalog 统一、上下文来源报告、固定路由与技能候选发布的边界；网关和定时调度由宿主组合。提案中的新增能力尚未实施。

## 模块与版本

模块路径：`github.com/kart-io/goagent-harness`；Go 1.27 及以上；go.mod 固定 GoAgent **v0.60.2-0.20261123144816-2a444fde983c**，对应远端不可变提交 `2a444fde983c`，包含新增记忆 API、公共 skills v3 依赖及 Runnable 取消收尾修复。无需workspace或replace即可消费；本地联调仍可显式使用workspace。该依赖为Go伪版本，没有创建正式发布标签。
本仓库由 GoAgent 拆出，源码位于 [kart-io/goagent-harness](https://github.com/kart-io/goagent-harness)，默认分支为 `main`。当前尚未发布版本标签，可克隆仓库运行测试与示例。
迁移来源与导入路径对照见 [迁移指南](docs/MIGRATION.md) 和 [NOTICE](NOTICE)。

## 结构

| 路径 | 职责 |
|---|---|
| [`planning`](planning/README_CN.md) | 目标解析、固定槽依赖执行、证据验收与有界重规划（[English](planning/README.md)） |
| `preset` | 配置加载、分层合并、组件装配、策略与沙箱校验、装配清单 |
| `tools/coding` | 工作区读写编辑、检索、命令、Git、待办 |
| `integration` | 通过公开 API 验证 Harness 与 GoAgent 持久图、审批和工具执行的组合 |
| `sandbox` | 文件/网络策略、Docker/本机执行、供应商无关 Provider 与受管会话 |
| `permissions` | Agent 会话权限、单次提权、角色模板与父子权限交集 |
| `examples/coding-agent` | 配置驱动的编码修复案例 |
| [`examples/planning-agent`](examples/planning-agent/README_CN.md) | 离线DAG、来源、重规划与持久等待案例（[English](examples/planning-agent/README.md)） |
| `examples/plan-agent` | 计划模式、钩子、压缩和运行中交互案例 |

## 使用

```go
import (
    "fmt"
    "github.com/kart-io/goagent/builder"
    "github.com/kart-io/goagent-harness/preset"
)

// client、ctx、root、host 由宿主提供。
sources, err := preset.FileSources(root, "")
if err != nil { return err }
set, err := preset.Load(sources...)
if err != nil { return err }
coder, ok := set.Get("coder")
if !ok { return fmt.Errorf("缺少 coder preset") }
assembly, err := preset.Build(ctx, coder, host)
if err != nil { return err }
defer assembly.Close()
agent, err := preset.Configure(builder.NewSimpleBuilder(client), assembly).Build()
if err != nil { return err }
output, err := agent.AsRunnable().Invoke(ctx, "完成任务")
```

配置格式、组件及生命周期见 [Preset 文档](preset/README.md)。宿主负责选择模型、会话 ID、预算、审批通道和持久化作用域；需要恢复时使用稳定执行键并核对 `Assembly.PinManifest`。

## 沙盒与 Agent 权限

沙盒采用 DSH 的策略与执行分层：文件模式、目录范围、网络策略独立表达；工具规则与人的审批不能越过宿主上限。
默认使用 `executor: native`（macOS Seatbelt，Linux 上 Bubblewrap 或 Landlock），保持 `network: false`；在容器中执行请写 `executor: docker` 并指定 `image`。
后端不可用或不能兑现策略时拒绝执行，不自动回退到无限制的本机执行。

```yaml
presets:
  - id: coder
    components:
      - id: workspace
        use: workspace
        config:
          executor: native
          sandbox: read-only
          maxSandbox: workspace-write
          network: false
      - {id: tools, use: coding-tools}
      - {id: permissions, use: permissions, config: {role: coder}}
```

`maxSandbox` 默认等于 `sandbox`，须显式配置才允许提权到更宽的模式。模型通过 `sandbox_permissions` 与 `justification` 请求一次具体调用的批准；不会改变整个会话。
宿主通过 `Assembly.Permissions.SetSession` 切换会话模式，通过 `Host.SessionStore` 持久化模式与版本，通过 `Host.SandboxCeiling` 设置审批也不能越过的硬上限。
每次模型请求附带当前策略事实；工具执行时仍会重新校验。

角色模板有 reader/reviewer、coder、coordinator、publisher；角色只贡献工具规则，不授予系统身份或外部服务权限。
普通子 Agent 受父 Agent 当前策略约束；只读 coordinator 如需调度可写 worker，应由宿主创建独立 worker。

无需模型或 API key 即可验证单次授权：

```bash
go run ./examples/sandbox-permissions -approve
```

预期依次输出 `first: success=false`、`approved: success=true`、`last: success=false`，临时演示文件在退出时清理。
本机沙盒主要限制文件修改和请求的网络行为，不保证宿主文件保密；它不隔离 Harness 本身、任意 Go 插件、MCP 服务或命令钩子。
Windows native 与 Landlock 尚未实现。平台验证范围和剩余限制见 [sandbox](sandbox/README.md)，授权与恢复契约见 [permissions](permissions/README.md)。

远端环境接入、Preset 配置、所有权/恢复及可运行案例见 [Sandbox 指南](docs/guides/SANDBOX.md)，职责边界见 [Sandbox 设计](docs/design/SANDBOX.md)。

## 验证

`go.mod` 声明的 GoAgent 依赖包含公共 `skills`、`tools/skill`、Memory API 与 Runnable 取消收尾修复。默认独立验证，推荐 Go 1.27.3：

```sh
GOWORK=off GOTOOLCHAIN=go1.27.3 go test -mod=readonly -race -count=1 ./...
GOWORK=off GOTOOLCHAIN=go1.27.3 go build -mod=readonly ./...
GOWORK=off GOTOOLCHAIN=go1.27.3 go vet -mod=readonly ./...
```

联仓开发可执行 `GOTOOLCHAIN=go1.27.3 make verify-local`：脚本先检查声明依赖，再用临时 workspace 验证相邻的 `../goagent`；临时 workspace 在退出时清理。`make test/race/build` 强制关闭 workspace。

可选 Docker 用例需配置镜像及相应用例开关。若 Colima 只共享用户目录，须将 `TMPDIR` 指向已共享目录，确保容器能读取测试工作区。Postgres 验证位于独立的 `integration/postgres` 模块，不包含在根模块 `./...` 中。

## 能力边界

- Harness 复用 GoAgent 的扩展点与持久日志，不新增独立执行循环。
- 子 Agent 内部不写持久日志，父 Agent 记录整次委派；中断后的处理按父调用策略执行。
- 持久运行中的 Redirect 降级为 Steer，RequestError 恢复钩子只在非持久运行中生效。
- Claude Code 适配覆盖文档列出的命令钩子，未声明完整产品或所有协议能力兼容。
- 文件系统协调与发布边界以所用 GoAgent 版本的 `harness/env` 契约为准。

采用 Apache-2.0 许可证，见 [LICENSE](LICENSE)。

Sandbox 补充资料：[API参考](docs/reference/SANDBOX_API.md)、[场景指南](docs/guides/SANDBOX_SCENARIOS.md)、[能力与选型调研](docs/research/2026-11-01-sandbox-capabilities.md)、[后续路线图](docs/design/SANDBOX_ROADMAP.md)、[合并验收](docs/evaluation/2026-11-01-sandbox-main/REPORT.md)。路线图中的新能力尚未实现。

Pi Agent设计对照：[固定源码调研](docs/research/2026-11-01-pi-agent-design.md)、[采纳计划](docs/design/PI_AGENT_ADOPTION_PLAN.md)、[230项选择性验证](docs/evaluation/2026-11-01-pi-agent/REPORT.md)。区分编码CLI与实验性Durable，不把会话分支视为文件回滚。

dsh-superagent 设计对照：[采纳计划](docs/design/DSH_SUPERAGENT_ADOPTION_PLAN.md)。外部副作用投递见 [effects](effects)，后台子工作见 [background](background)。
