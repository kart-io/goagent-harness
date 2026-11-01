# 从 GoAgent 迁入独立 Harness

## 包路径

| 原路径 | 新路径 |
|---|---|
| `github.com/kart-io/goagent/harness/preset` | `github.com/kart-io/goagent-harness/preset` |
| `github.com/kart-io/goagent/tools/coding` | `github.com/kart-io/goagent-harness/tools/coding` |

`preset.Configure`、`Load`、`Build`、`Assembly` 和编码工具 API 保持原有行为，迁移时改写 import。
配置文件仍为 `~/.goagent/agents.yml`、`.goagent/agents.yml` 和 `.goagent/agents.local.yml`，本次不更改配置语法与存储位置。

## 保留在 GoAgent

Builder、Runnable、Graph、durable、budget、observe、Dispatcher/Pipeline、Handle、循环钩子、`harness/env`、`tools/policy`、`tools/spill` 继续由 GoAgent 提供。已有业务编排用户不需要引入本项目。

## 依赖与验证

当前模块依赖已发布的 GoAgent v0.32.2，包含 Preset 所需循环扩展点与当前执行环境接口；最初拆分基线为 v0.23.0。后续独立版本发布须更新明确的 GoAgent 版本并分别运行 `GOWORK=off go test ./...` 和本地跨仓验证脚本。
源码已迁至 `github.com/kart-io/goagent-harness` 的 `main` 分支，尚未发布版本标签。不在 go.mod 中写入本地 replace；需要稳定版本的使用方应等待正式标签。

发布依赖与本地 checkout 均需分别验证，文件系统保证以各自具体代码为准。Sandbox 供应商适配器为独立可选模块，其临时 workspace 和单独验收命令见 [Sandbox 指南](guides/SANDBOX.md)。

## 示例与设计

`examples/tools/09-coding-agent` 迁至 `examples/coding-agent`；`examples/tools/10-plan-agent` 迁至 `examples/plan-agent`。
历史设计见 [Harness 核心化设计](design/HARNESS_CORE_DESIGN.md)，迁移来源见 [NOTICE](../NOTICE)。
