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

新模块依赖已发布的 GoAgent v0.23.0，包含 Preset 所需循环扩展点。后续独立版本发布须更新明确的 GoAgent 版本并分别运行 `GOWORK=off go test ./...` 和本地跨仓验证脚本。
本次不发布版本或远程仓库，不在 go.mod 中写入本地 replace。未发布前以本地源码验证，不能假定远程模块已可下载。

GoAgent 当前 checkout 还包含 v0.23.0 之后的 LocalFS 修复；本地 workspace 验证该实现，发布依赖验证基线版本，二者的文件系统保证应分别以对应代码为准。

## 示例与设计

`examples/tools/09-coding-agent` 迁至 `examples/coding-agent`；`examples/tools/10-plan-agent` 迁至 `examples/plan-agent`。
历史设计见 [Harness 核心化设计](design/HARNESS_CORE_DESIGN.md)，迁移来源见 [NOTICE](../NOTICE)。
