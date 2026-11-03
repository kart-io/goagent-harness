# preset — 声明式自定义 Agent

一个 preset 就是一个 Agent：由若干组件行组成，组件列表即该 Agent 的全部能力（提示词、工作区与沙箱、工具、权限、结果溢出）。配置分层叠加，但权限上限只由可信层决定，项目里的配置只能收紧权限。设计与取舍见 [设计文档第 14 节](../docs/design/HARNESS_CORE_DESIGN.md)。

```go
sources, _ := preset.FileSources(root, userHome)         // 用户层（可信）、项目层与本地层（不可信）
set, err := preset.Load(append(sources, hostLayer)...)  // 合并并严格校验；不启动进程、不读秘密
coder, _ := set.Get("coder")
asm, err := preset.Build(ctx, coder, preset.Host{Root: root, Approver: approver, Trust: &preset.FileTrustStore{Path: trustFile}})
var trustErr *preset.TrustRequiredError
if errors.As(err, &trustErr) {
    // 向用户展示 trustErr.Items，同意后授权再重新 Build
    _ = host.Trust.Grant(trustErr.Root, trustErr.Digest)
}
defer asm.Close()

// 系统提示词、工具与组件贡献的循环扩展点（计划模式、压缩、Claude Code 的提交与停止钩子）
agent, _ := preset.Configure(builder.NewSimpleBuilder(client), asm).
    WithSessionID(sessionID).
    Build()
```

完整示例见 [examples/coding-agent](../examples/coding-agent)。

## 配置格式

```yaml
limits:                       # 只能出现在可信层
  sandbox: workspace-write    # 不可信层的沙箱模式上限
  executors: [docker]         # 缺省不限
  images: [golang:1.25]       # 缺省不限
  network: false
  hostProcesses: []           # 允许在宿主机启动的进程
  secrets: [GITHUB_TOKEN]     # 允许引用的秘密
presets:
  - id: coder
    name: 编码助手
    components:
      - {id: persona, use: persona, config: {prompt: "你是一个严谨的编码 Agent……"}}
      - {id: workspace, use: workspace, config: {sandbox: workspace-write, executor: docker, image: golang:1.25}}
      - {id: instructions, use: instructions}
      - {id: coding, use: coding-tools}
      - {id: permissions, use: permissions, config: {base: coding, rules: [{tool: git_commit, decision: ask}]}}
      - {id: spill, use: spill}
```

| 层 | 位置 | 信任 |
|---|---|---|
| 内置、宿主 | 代码传入的 `Source{Trusted: true}` | 可信 |
| 用户 | `~/.goagent/agents.yml` | 可信 |
| 项目 | `<root>/.goagent/agents.yml` | 不可信 |
| 本地 | `<root>/.goagent/agents.local.yml` | 不可信（位于仓库目录内） |

### 合并

- preset 按 `id` 合并：后层出现的 `name`/`description` 替换前层，写空串即清空；`remove: true` 删除整个 preset。
- 组件按 `id` 合并：同 id 整行替换并保留位置，`config` 整体替换不深合并；替换不得改变 `use`（确需更换实现写 `replace: true`）；`remove: true` 删除，之后重新出现时追加到末尾；新 id 按层、文件内顺序追加；同一层重复 id 报错。
- 提示词段落与钩子按最终组件顺序排列；依赖只影响初始化顺序。
- 每个配置源只接受一个 YAML 文档；尾随第二个文档或语法错误在 `Load` 时报错。
- 未知字段、未知组件、单例重复、缺少依赖都在 `Load` 时报错。`set.Dump()` 输出合并结果、每行来源、被覆盖与被删除的行，以及需要授权的扩权项。

### 信任

不可信层中的下列内容是**扩权项**，`Build` 返回 `*TrustRequiredError`，经 `TrustStore.Grant` 授权后才生效：

- 超出 `limits` 的沙箱模式、执行器、镜像、联网、宿主进程、秘密引用；
- `allow` 规则、`base: coding`、`sessionTools`、角色模板、额外可写目录与环境变量传递；
- 替换或删除来自可信层的组件（可信层可用 `locked: true` 直接禁止）。

新增 `deny`/`ask` 规则、降低沙箱模式、删除自己层的组件无需授权。授权绑定项目根目录与扩权项（含所在组件的完整配置）的摘要，配置变化后需重新授权。

## 内置组件

| 组件 | 配置 | 说明 |
|---|---|---|
| `persona` | `prompt` | 一段系统提示词 |
| `instructions` | `files`（默认 AGENTS.md、CLAUDE.md）、`maxBytes`（默认 32768） | 读取工作区内的项目说明，依赖 `workspace` |
| `workspace` | `sandbox`、`maxSandbox`、`executor`（docker/local/native）、`image`、`network`、`additionalWritableRoots`、`envAllowlist`、`commandTimeout`、`maxOutputBytes`、`linters` | 单例。沙箱模式是硬性要求，见下表 |
| `coding-tools` | `tools`（缺省为沙箱下可用的全部） | 依赖 `workspace`；列出不可用的工具时 Build 失败 |
| `permissions` | `approval`（ask/never）、`role`、`base`（coding）、`rules`、`sessionTools` | 可多行，全部规则合并为一个策略，按 Deny > Ask > Allow 取最严格者 |
| `spill` | `maxBytes`（默认 100000）、`retention`（默认 168h） | 超长结果写入 `Host.ArtifactDir`，提供 `read_spill` |
| `mcp` | `transport`（stdio/streamable-http）、`command`、`args`、`env`、`url`、`headers`、`tools`、`aliases`、`startupTimeout`（默认 30s） | 可多行，见下文 |
| `claude-code-hooks` | `path`、`hooks`、`toolNames`、`timeout`（默认 60s） | 单例。Claude Code 命令钩子，见下文 |
| `skills` | `dirs`（默认 .agents/skills、.claude/skills） | 单例。列出技能并提供 `load_skill` |
| `subagent` | `preset`、`toolName`、`description`、`toolFilter`、`maxDepth`（默认 1）、`timeout`（默认 10m） | 可多行。把任务交给另一个 preset 装配的子 Agent |
| `plan-mode` | `initial`（默认 true）、`allow`（缺省为只读工具） | 单例。计划模式，见下文 |
| `compaction` | `maxTokens`（默认 100000）、`keepTokens`（默认 maxTokens/4）、`instructions` | 单例。对话压缩，需要 `Host.CompactionModel`，见下文 |
| `verify-before-stop` | `command`（必填）、`maxAttempts`（默认 3） | 单例，需要 `workspace`。模型给出最终答复前在工作区运行检查命令，失败时把输出交还模型续跑；本会话还没有经写入类工具改动文件时不检查 |

| 沙箱模式 | 文件工具 | 命令（含 git、lint） |
|---|---|---|
| `read-only` | 拒绝写入；若 maxSandbox 允许提权则仍提供写入工具供单次请求批准 | Docker 只读挂载或 native OS 约束；legacy local 不提供命令 |
| `workspace-write` | 写入限于工作区和显式命名的额外目录 | Docker 或 native；不隔离的 local 执行器 Build 失败 |
| `danger-full-access` | 写入限于工作区 | 可在宿主机执行 |

执行器的网络能力必须与 `network` 一致（本地执行器总能联网，须写 `network: true`），做不到即 Build 失败，不静默降级。

`maxSandbox` 默认等于 `sandbox`，规定人可以批准的最高文件模式；网络和目录不会随模式自动扩大。
额外目录填写物理绝对路径；文件工具通过 `@root1/path` 等相对范围访问，命令使用执行端路径（Docker 主工作区为 `/workspace`）。
native 的文件读取仍受宿主账户权限影响，不承诺凭据保密；文件工具继续保留显式路径范围。
Linux 的 native 优先用 Bubblewrap，不可用时回退到 Landlock（隔离更弱，见 [sandbox](../sandbox/README.md)），manifest 的 `contract` 和 `enforcement` 会反映这一点。
选择 native 时 Build 实际探测后端；Docker 为兼容现有装配流程，在 Run 前探测，未启动时 manifest 标记 `unverified`。
因此 Build 成功本身不能作为 Docker 当前可用的证据。两种后端执行前都会重新检查。

宿主权限 API：

```go
asm, err := preset.Build(ctx, p, preset.Host{
    Root: root,
    AgentID: "coding-worker",       // 稳定可信身份；默认 preset id
    Approver: approver,
    SessionStore: sessionStore,      // 可选；持久保存 mode/revision，不保存审批
    SandboxCeiling: &hostCeiling,    // 可选；批准与项目 trust 均不能越过
})
if err != nil { return err }
defer asm.Close()
err = asm.Permissions.SetSession(ctx, "session-1", sandbox.ReadOnly)
```

无 workspace 时 `Assembly.Permissions` 为 nil；调用前须检查。`SetSession` 仅供宿主使用，不能注册为模型工具。
每次模式切换递增版本，令尚未开始执行的旧批准失效。单次提权参数须成对提供 `sandbox_permissions` 和 `justification`，
并有宿主提供的 SessionID / CallID；批准绑定调用与策略版本，不提供整个会话的提权。
`approval: never` 同样拒绝沙盒提权，工具的 allow 规则不能覆盖这一限制。

| 角色模板 | 默认工具规则 |
|---|---|
| reader / reviewer / coordinator | 允许读取与检索；禁止编辑、命令和 Git 子进程 |
| coder | 允许文件编辑；命令和 Git 需审批；禁止模板中声明的发布动作 |
| publisher | 发布、推送、消息等仍需宿主审批；不自动授予外部资源范围 |

角色模板对未匹配的工具仍遵循默认 ask，不能替代外部服务的资源授权。
Git 读取工具也可通过仓库配置或辅助程序执行子进程，所以只读角色不会默认放行它们。

宿主基线：没有任何规则匹配的调用需要审批；`approval: never` 时 Ask 自动拒绝；未提供 `Approver` 时 Ask 的结果为 Unavailable，同样按拒绝处理。

## MCP

```yaml
- {id: github, use: mcp, config: {transport: stdio, command: github-mcp, env: {GITHUB_TOKEN: {secret: GITHUB_TOKEN}}}}
- {id: docs, use: mcp, config: {transport: streamable-http, url: "https://example.com/mcp", headers: {Authorization: {secret: DOCS_AUTH}}, tools: [search]}}
```

- 工具名为 `mcp__<组件 id>__<远端工具名>`，`aliases` 可改名，最终名称须全局唯一且匹配 `^[A-Za-z0-9_-]{1,64}$`；权限规则按最终名称匹配。
- 工具列表在 Build 时取快照；服务名称与版本、每个工具的 schema 与描述摘要记入装配清单。
- 远端返回的错误结果作为拒绝交给模型；连接错误中止调用。持久运行中中断或失败的 MCP 调用不会在恢复时重跑。
- stdio 服务在宿主机启动、不受沙箱约束，只继承 `PATH`、`HOME`、`USER`、`LANG`、`TMPDIR`，其余环境变量须在 `env` 中列出。
- 来自不可信层时：stdio 属于"在宿主机启动进程"（`limits.hostProcesses` 按命令名放行），streamable-http 属于"联网"。
- `Assembly.Close` 关闭会话并结束 stdio 进程；Build 中途失败时已连接的会话同样被关闭。

## Claude Code 钩子

```yaml
- {id: hooks, use: claude-code-hooks, config: {path: .claude/settings.json}}
```

- 支持 `PreToolUse`、`PostToolUse`、`UserPromptSubmit`、`Stop`、`SessionStart` 的 `command` 钩子，协议与 Claude Code 相同：命令经 `sh -c` 在工作区根目录执行（设置 `CLAUDE_PROJECT_DIR`），stdin 为 `session_id`、`cwd`、`hook_event_name` 与各事件字段的 JSON（工具事件有 `tool_name`、`tool_input`，调用后另有 `tool_response`）；`transcript_path` 总为空串。
- 调用前：退出码 2 阻止调用、stderr 作为原因交给模型；stdout 的 `hookSpecificOutput.permissionDecision`（allow/ask/deny）或 `decision`（approve/block）给出裁决。多个钩子按 deny > ask > allow 合并，并与权限规则一起聚合：钩子的 allow 不越过规则的 ask。
- 调用后：退出码 2、`decision: block` 的原因与 `additionalContext` 作为附加上下文交给模型（工具已经执行）。
- `UserPromptSubmit`（stdin 另有 `prompt`）：本步新收到的每条用户输入各执行一次，包括轮次输入与运行中 Steer、Inject、Redirect 的输入。退出码 2 或 `decision: block` 去掉该输入；轮次输入被阻止时不请求模型，以原因作为答复结束本轮。stdout 纯文本或 `additionalContext` 追加在该输入之后。
- `Stop`（stdin 另有 `stop_hook_active`、`last_assistant_message`）：模型给出最终答复时执行；退出码 2 或 `decision: block` 把原因作为 user 消息续跑。本轮已因 Stop 钩子续跑过时 `stop_hook_active` 为 true（用户 Steer 不算），钩子应据此避免无限续跑。续跑后至少还要剩一步带工具的请求，否则不执行 Stop 钩子，答复的元数据记 `turn_stopping_skipped`。
- `SessionStart`（stdin 另有 `source: startup`，`matcher` 匹配 source）：每个会话第一次运行时执行一次，stdout 纯文本或 `additionalContext` 进入 system 提示词，同一会话之后的轮次沿用。
- 其他非零退出码、无法启动与超时不阻止。`UserPromptSubmit`、`Stop`、`SessionStart` 经循环扩展点实现，只在原生工具流中生效（`llm.ToolStreamClient` 且配置了工具）。
- 编码工具在钩子中以 Claude Code 的名称出现（`run_command` → `Bash`、`read_file` → `Read`、`write_file` → `Write`、`edit_file` → `Edit` 等），文件类工具的参数另带 `file_path`；`matcher` 按完整匹配的正则同时匹配两种名称。
- `path` 文件在 Build 时读取，其中不支持的事件与类型被忽略。文件属于工作区内容：即使本行来自可信层，文件中的命令也需要授权（或列入 `limits.hostProcesses`），文件改动后授权失效。内联的 `hooks` 来自不可信层时同样是扩权项。

## 技能

`<目录>/<名称>/SKILL.md`，以 YAML frontmatter 开头（`name` 须与目录名一致，`description` 必填，`disable-model-invocation: true` 时不列入提示词）。系统提示词列出技能的名称与描述，模型用 `load_skill` 读取说明（Build 时读取）与技能目录中的其他文件；读取限定在技能目录内。同名技能以先出现的目录为准。不可信层读取工作区外的目录（绝对路径或 `~`）需要授权。

技能发现与读取使用 `os.Root` 限定目录范围。`SKILL.md` 和参考文件可使用指向技能目录内部的相对符号链接，越界链接和绝对链接会被拒绝。Build 持有每个技能的目录句柄，目录被重命名或原路径被替换后仍读取原目录；`Assembly.Close` 和装配失败会释放句柄。skills 组件版本已升至 2，使用旧版本技能装配清单的持久运行需新建会话。

## 子 Agent

```yaml
presets:
  - id: lead
    components:
      - {id: review, use: subagent, config: {preset: reviewer, toolName: ask_reviewer, toolFilter: {allow: [read_file, grep]}}}
  - id: reviewer
    components: [...]
```

- 子 Agent 与父 Agent 一同装配，由宿主的 `Host.RunAgent` 运行（`preset.BuilderRunner(client, maxIterations)` 用 builder 运行）。
- 权限取交集：子 Agent 的每次调用先经过父 Agent 的调用前钩子与守卫（含父 Agent 的权限规则），模式、目录与网络和环境变量传递不得超过父 Agent；实时会话策略和祖先版本也会校验，父 Agent 的 `approval: never` 同样适用。
- `toolFilter` 在初始化之前生效：被完全过滤掉的组件（如允许列表中没有其工具的 MCP 服务）不初始化。允许列表引用不存在的工具时 Build 失败。
- preset 引用有环时 Load 失败；嵌套深度超过 `maxDepth` 时 Build 失败。关闭父 Agent 同时关闭子 Agent。
- `mode: background` 时工具立即返回 `child_id`，子 Agent 由 `Host.Background`（[`background.Manager`](../background)）持有，有稳定身份，重启后找回，完成后通知所有者。需要持久运行提供的 `ExecutionKey`。权限与前台相同。
  - 另提供 `<toolName>_status` 查询工具，只看得到当前所有者的子工作；它和其他工具一样需要权限规则放行。
  - 不调用 `Host.Approver`：需要审批时子工作进入 `waiting` 并通知所有者。宿主用 `ApprovalOf` 取出调用内容，用 `Manager.Resume` 和 `ApprovalAllow`、`ApprovalDeny` 答复，其他值按拒绝处理。
  - 目标 preset 使用托管沙箱时，`Build` 不分配实例：每个 child 运行时分配自己的实例，重启后连回，结束时销毁。`Registry.Namespace` 须在重启后保持不变，Provider 须支持 `Connect`。宿主用 `Resolve` 借出或保留的实例由宿主负责。
  - 实例在分配时崩溃：Provider 实现 `sandbox.InstanceFinder` 时按 Scope 查回；没有实现时子工作失败（`sandbox_unknown`），不会再分配一个，这条释放进入 `Manager.Problems` 由宿主处理。`Create` 返回错误但没有声明 `sandbox.ErrNotCreated` 时同样按结果未知处理：响应可能只是丢了。
  - `Build` 时托管沙箱用占位环境校验，`CheckPolicy` 推迟到每次运行：后端不满足策略时子工作失败，`Build` 不报错。
  - 前台子 Agent 位于后台链路中时保留日志，审批等待能穿到外层续跑；宿主自定义 `RunAgent` 时须在有日志时启用持久范围，并按需要开启 `WithInterruptedModelResend`，可参考 `BuilderRunner`。
- 子 Agent 不写持久日志：父 Agent 的日志记录整次委派，中断的委派按父调用的策略整体重跑。

## 计划模式

```yaml
- {id: plan, use: plan-mode}                     # 会话开始即进入计划模式
- {id: plan, use: plan-mode, config: {initial: false, allow: [read_file, grep, mcp__docs__search]}}
```

- 计划模式中只能调用 `allow` 中的工具（缺省为 `read_file`、`glob`、`grep`、`git_status`、`git_diff`、`git_log`、`todo_write`、`read_spill`、`load_skill`），其余调用被拒绝并提示模型先提交计划。
- 模型调用 `exit_plan_mode` 提交计划，总是需要审批（项目层的 allow 越不过）：批准后退出计划模式，拒绝时仍在计划模式；`approval: never` 时无法退出。`enter_plan_mode` 无需审批。
- 处于计划模式时每步附加一段运行上下文说明；切换模式不增删工具，system 前缀不变。
- 模式按会话记录，每步开始时从对话中最近一次成功的切换结果恢复（只认这两个工具的结果），因此 Followup 与持久运行恢复后的后续步骤状态一致；对话中没有切换结果（如只带文本的记忆历史）时沿用进程内状态，进程内也没有时取 `initial`。持久运行恢复时，中断那一步中尚未完成的调用按进程内状态或 `initial` 判断。

## 压缩

```yaml
- {id: compact, use: compaction, config: {maxTokens: 60000, instructions: "保留所有文件路径"}}
```

```go
preset.Build(ctx, p, preset.Host{..., CompactionModel: cheapClient})
```

- 每步请求前估算对话长度（UTF-8 字节数的三分之一，对英文偏保守），超过 `maxTokens` 时把较早的消息交给 `Host.CompactionModel` 写成一条 `[此前对话的摘要]` user 消息；system、最近约 `keepTokens` 的消息与本步新收到的输入原样保留，保留区从非 tool 消息开始，不会拆开工具调用与其结果。
- 模型请求因超出上下文失败时（`llm.ClassifyError` 的 `ShouldCompress`）强制压缩后重试，只在非持久运行中生效。
- 持久运行中压缩随该步的 PreStep 记入日志，恢复时回放摘要，不再调用摘要模型。

## 持久运行

```go
asm, _ := preset.Build(ctx, coder, host)
if err := asm.PinManifest(runCtx); errors.Is(err, preset.ErrManifestChanged) {
    // 配置在中断后发生了变化：新建会话，不在旧日志上继续
}
agent.Invoke(runCtx, input)
```

`PinManifest` 在首次运行时把装配清单摘要写入日志，恢复时比对；`runCtx` 是交给 Agent 的同一个带日志的 ctx。

## 自定义组件

```go
type MyConfig struct {
    Endpoint string        `yaml:"endpoint"`
    Token    preset.Secret `yaml:"token"` // 配置中写作 {secret: NAME}
}

func init() {
    preset.Register(preset.Spec[MyConfig]{
        Name: "my-tool", Version: "1",
        Needs: func(c *MyConfig) preset.Needs { return preset.Needs{Network: true} },
        Build: func(ctx context.Context, a *preset.Assembler, c *MyConfig) error {
            token, err := a.Secret(c.Token)
            if err != nil {
                return err
            }
            a.AddTools(newMyTool(c.Endpoint, token))
            a.OnClose(func() error { return nil })
            return nil
        },
    })
}
```

秘密只能经 `Secret` 或 `Value`（字面量或 `{secret: NAME}`）类型的字段引用，Build 时用 `a.Secret`/`a.Value` 解析；`Dump`、错误与 `Assembly.Manifest` 中只出现引用。`Manifest.Digest()` 覆盖组件配置与版本、工具 schema、提示词与工作区，可用于在恢复持久运行前核对执行语义。

## 测试

```bash
go test ./preset/ ./integration/
```

## 远端 Sandbox Provider

`workspace.config.executor: sandbox` 通过宿主 `Host.Sandboxes` 选择命名 Provider；配置还需 `provider`、`image`、`network`。没有内置远端供应商实现；命令要求实际执行器实现 `sandbox.PolicyExecutor`，严格只读文件视图不开放命令或 linter，附加宿主目录与环境变量映射当前拒绝。命令和文件绑定同一个远端实例，Host.Root 不会隐式上传或挂载。宿主负责 Registry.Close，包含装配失败和清理重试。子 Agent 默认独立实例，并在分配前校验父级权限与网络上限。恢复必须重连原实例并核对真实 ID 的 Manifest。完整配置、后端策略强制、实例所有权和代码见 [Sandbox 指南](../docs/guides/SANDBOX.md) 与 [设计文档](../docs/design/SANDBOX.md)。
