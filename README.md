# GoAgent Harness

基于 [GoAgent](https://github.com/kart-io/goagent) 的可配置 Agent 装配与运行组件库。面向需要在 CLI、IM 助手或 HTTP 服务中嵌入工程 Agent 的 Go 开发者。

GoAgent 提供模型与工具循环、类型化工作流、持久恢复、预算、观测和执行环境接口；本项目提供 Preset 装配、编码工具、计划模式、技能、子 Agent 配置、Claude Code 命令钩子与上下文压缩。两个项目单向依赖，不复制执行引擎。

## 模块与版本

模块路径：`github.com/kart-io/goagent-harness`；Go 1.25 及以上；GoAgent 基线为 **v0.23.0**。
本仓库由 GoAgent 拆出，源码位于 [kart-io/goagent-harness](https://github.com/kart-io/goagent-harness)，默认分支为 `main`。当前尚未发布版本标签，可克隆仓库运行测试与示例。
迁移来源与导入路径对照见 [迁移指南](docs/MIGRATION.md) 和 [NOTICE](NOTICE)。

## 结构

| 路径 | 职责 |
|---|---|
| `preset` | 配置加载、分层合并、组件装配、策略与沙箱校验、装配清单 |
| `tools/coding` | 工作区读写编辑、检索、命令、Git、待办 |
| `integration` | 通过公开 API 验证 Harness 与 GoAgent 持久图、审批和工具执行的组合 |
| `sandbox` | 文件策略、命名目录范围与 Docker / 本机 OS 执行适配器 |
| `permissions` | Agent 会话权限、单次提权、角色模板与父子权限交集 |
| `examples/coding-agent` | 配置驱动的编码修复案例 |
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
默认仍使用 Docker，并保持 `network: false`。选择 `executor: native` 可使用 macOS Seatbelt 或 Linux Bubblewrap。
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

## 验证

```bash
# 仅使用 go.mod 中发布的 GoAgent 版本，不依赖本地源码。
GOWORK=off go test ./...
GOWORK=off go build ./...

# 同时验证本地两个项目；参数可指定 GoAgent checkout，默认 ../goagent。
./scripts/verify-goagent.sh ../goagent

# 可选 Docker 案例，需要本地已有含 sh 的镜像。
GOAGENT_CODING_DOCKER_IMAGE=alpine:3 go test -run Docker ./tools/coding
```

脚本临时创建 go.work 并在退出时清理，不修改两个项目的 go.mod 或现有 go.work。
`make test`、`make race`、`make build` 验证独立模块；`make verify-local` 验证本地组合。

## 能力边界

- Harness 复用 GoAgent 的扩展点与持久日志，不新增独立执行循环。
- 子 Agent 内部不写持久日志，父 Agent 记录整次委派；中断后的处理按父调用策略执行。
- 持久运行中的 Redirect 降级为 Steer，RequestError 恢复钩子只在非持久运行中生效。
- Claude Code 适配覆盖文档列出的命令钩子，未声明完整产品或所有协议能力兼容。
- 文件系统协调与发布边界以所用 GoAgent 版本的 `harness/env` 契约为准。

采用 Apache-2.0 许可证，见 [LICENSE](LICENSE)。
