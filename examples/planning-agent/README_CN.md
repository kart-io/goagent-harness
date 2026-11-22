# 离线规划示例

[English](README.md)

在 Harness 仓库根目录用仓库指定的 Go 工具链运行。每个状态目录对应一次运行。

```sh
go run ./examples/planning-agent -scenario dag -state-dir "$(mktemp -d)"
go run ./examples/planning-agent -scenario sources -state-dir "$(mktemp -d)"
go run ./examples/planning-agent -scenario replan -state-dir "$(mktemp -d)"
```

| 场景 | 预期结果 |
| --- | --- |
| `dag` | 提取并验收北站计数，再生成总数为 5 的报告。 |
| `sources` | 第一批在两个槽位分别提取北站和南站计数。两项均验收后，下一批生成总数为 12 的报告。 |
| `replan` | 最终审查要求采用北站更正材料。第 2 版计划复用已验收的南站产物，让旧北站产物和旧报告失效，再生成总数为 15 的报告。 |
| `wait` | 在持久化输入点暂停，收到明确答复后记录确认。 |

来源是嵌入程序的合成 JSON。提取产物的引用定位到原始材料的 `/count`，报告的引用定位到已验收提取产物的 `/count`。宿主在验收前补齐来源摘要和引用值摘要。本地审查另行核对提取值和报告总数。

## 恢复等待中的运行

```sh
state_dir="$(mktemp -d)"
go run ./examples/planning-agent -scenario wait -state-dir "$state_dir"
```

第一条命令打印 `status: waiting` 后退出。把 JSON 输出中 `interrupts[0].key` 的值复制到变量 `resume_key`，再运行：

```sh
go run ./examples/planning-agent -scenario wait -state-dir "$state_dir" \
  -resume-key "$resume_key" -answer true
```

等待点公布的 schema 为 `{"type":"boolean","const":true}`。按命令传入 JSON 布尔值 `true`，字符串 `"true"` 会被拒绝。用同一目录重新执行第一条命令可以查看持久化等待及其叶子中断键。实际部署时，宿主须先认证答复者并授权具体动作。中断键仅定位续跑点。

## 状态、权限与成本

- 文件存储保存图检查点、操作日志和预算账本。完成后输出规划 State，其中包含已验收产物和历史尝试。已失效的产物留在历史记录中，后续任务无法读取它们作为输入。业务结果通过 `outcome.status` 表达，执行错误写入 stderr 并返回非零退出码。
- State 中包含有界来源 JSON 的冻结副本和派生产物。实际导入的源文件和原始记录遵循独立的存储与生命周期规则。删除专用示例状态目录会丢弃本地续跑信息和预算账本，嵌入的材料和独立保存的源记录仍在，外部副作用也无法借此撤销。
- 示例使用宿主信任的本地只读成员，无网络、命令、工作区写入或模型后端。实际模型调用为零，无需 API 密钥。真实宿主须执行自己的工作区、工具、网络和审批权限，规划依赖和验收条件本身不授予访问权限。
- 示例上限为 8 个任务、8 批、2 次重规划、每任务 2 次尝试、1 轮目标澄清。单个产物上限 1 KiB，产物合计上限 8 KiB，State 上限 128 KiB。持久根预算的模型请求、工具调用和 Agent 调用各限 8 次，预留 token 上限 4,096。本地确定性回调不消耗这些请求和调用额度。
- 恢复须使用同一目录、同一场景和兼容的程序版本。图会检查工作流、状态、规划器、审查器、成员及配置版本，账本也会校验根预算策略。恢复旧检查点时请使用匹配的旧程序和配置。切换到不兼容版本或其他场景时请使用新目录。
