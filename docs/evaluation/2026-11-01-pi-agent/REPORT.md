# Pi Agent 设计调研验证报告

日期：2026-11-01。固定Pi源码：`2c8d1feb147f6b3d97e855da803bfe7b19f3c403`，规范仓库`earendil-works/pi`；旧`badlogic/pi-mono`地址跳转到此处。package.json标注1.4.0，不将源码版本等同于npm发行版验证。Harness基线`4782150`，GoAgent依赖v0.32.2。

设计结论见[调研](../../research/2026-11-01-pi-agent-design.md)，待采纳事项见[计划](../../design/PI_AGENT_ADOPTION_PLAN.md)。本轮没有修改GoAgent/Harness生产执行代码、Go模块依赖或默认权限。

## 方法

从官方Git仓库取得固定源码，直接运行所选原始测试；没有复制一套简化Agent循环替代上游实现。Node v25.2.1、Vitest 4.1.12。测试依赖安装在临时目录，使用`--ignore-scripts`，不全局安装Pi，不启动交互CLI。

Agent测试使用受控StreamFn，Durable测试使用faux模型流、MemoryStorage/SQLite及受控恢复情景；NodeExecutionEnv一致性测试使用本机临时目录和进程。没有向模型供应商发送请求。另有一个自有场景探针直接使用实际CLI SessionManager，验证会话分支与文件副作用的区别。

## 已执行的验证

| 组 | 来源/用途 | 通过数量 | 输出 |
| --- | --- | --- | --- |
| Agent核心 | 原始agent-loop.test.ts、agent.test.ts；消息转换、事件、队列、钩子、并发与结束边界 | 79 | [agent.log](agent.log) |
| Durable | 原始memory-storage、session-forks、harness-tools-recovery、harness-ownership；提交、恢复、策略和所有权 | 99 | [durable.log](durable.log) |
| 本机环境 | 原始env-node-conformance；文件/命令/取消等本机环境契约 | 51 | [env-node.log](env-node.log) |
| CLI会话树场景 | 实际SessionManager中切换分支，模型投影改变但磁盘已写内容不回滚 | 1 | [session-probe.log](session-probe.log) |
| 合计 | 选择性验证，**不是全仓测试** | **230** | 以上各组 |

分支探针记录了正常assistant toolCall/toolResult配对，再选择另一个分支；没有运行真实模型。源码见[session-probe.test.ts.txt](session-probe.test.ts.txt)。该探针验证内存中的CLI会话树行为，不证明其JSONL文件断电恢复，也不证明任意业务副作用会自动回滚。

Durable组包含SQLite重新打开、safe/unsafe策略变化、中断后的部分输出和owned子工作的取消。测试受控地制造相关状态，不等同于真实kill -9、硬件断电或所有存储后端的故障演练。Node会对node:sqlite发出experimental提示，此处记录该运行环境限制。

## 初始设置问题及处理

首次同时运行Agent两文件时，agent-loop的38项通过，但agent.test.ts因缺少Git不跟踪的供应商模型数据而无法加载，见[初始日志](setup-initial-agent.log)。按上游固定的`nix/model-catalog.json`取得对应catalog，验证SHA256，再使用官方hydrate脚本生成数据，之后79项通过。这不是产品行为失败，也没有用伪造数据填充目录。

固定catalog revision：`sha256-7fe1b41feaf43a7aad4bc832e6b306c7ced56e6d41bbc724b74e91eeacff6edf`。URL和实际checksum保存在[sources.json](sources.json)。没有调用模型API生成目录。

一次扩展范围的测试尝试中，Node环境51项通过，但编码工具file-mutation-queue套件因最小依赖环境缺少cross-spawn而未加载，见[初始环境日志](setup-initial-env.log)。不把它算作行为回归或成功验收。随后补齐固定cross-spawn供实际SessionManager探针使用，并单独重跑Node环境组。完整编码CLI/渲染器工具套件没有验证。

首次打包复跑环境还发现macOS /tmp别名路径下反复npm --prefix安装生成了不可移植lock路径；这属于调研工具设置问题。已在规范路径的干净目录重建lock，并让复跑临时根目录先resolve后安装，不以改动Pi实现来绕过问题。

## 已核对但没有实际部署的部分

- Rust pi-env daemon、SSH部署、连接中断后的远端进程清理、PTY及其跨平台行为：按固定官方协议/源码分析，没有远端验收。
- Docker/OpenShell/Gondolin等整个Agent或工具级隔离：按官方文档核对，未在本轮部署。
- 扩展热加载、project trust、MCP和完整CLI交互：源码分析，不从核心测试通过推导为产品集成已验证。
- Code Mode确定性重放与任务图：按实验性Durable设计分析，未跑完整Code Mode/worker或真实外部副作用测试。
- 存储断电保证、生产并发容量、模型效果和性能比较：未验证，不给出相关SLA。

## GoAgent侧核对

直接核对依赖模块v0.32.2的Builder钩子、Handle、Dispatcher、core/session.Store.Fork与durable.OperationPolicy；核对当前Harness的Loop、Compaction、Subagent和Sandbox装配。确认现有能力后才列采纳差距，不把Steer、Fork、workflow或持久操作重新描述成待新建的API。

关键差别：当前preset子Agent禁用子层journal，父调用记录整体委派；Pi实验性Durable的owned子Conversation/Task具有另一套持久所有权。Harness 对后端的工作区 Root、条件写及原子写能力必须分别核验，不能把 Pi 的 Node 式执行语义直接当作相同保证。此处是接口要求，不代表当前交付了远端供应商适配器。

## 复跑

```sh
python3 docs/evaluation/2026-11-01-pi-agent/reproduce.py --output /tmp/harness-pi-verification
```

需要Git、Python3、Node.js（上游要求至少22.19）和npm，可访问GitHub、npm及固定Pi catalog。脚本临时取得指定提交，使用归档依赖lock、官方模型数据hydration与归档的source-alias测试配置，执行四组并检查上游跟踪源码未改变；临时目录结束后清理。不需要Pi账号或模型密钥，也不在本项目安装node_modules。

配置只把工作区包导向上游源码并加载上游测试setup，没有替换Agent/Durable实现。原文件为[vitest.config.ts.txt](vitest.config.ts.txt)，依赖为[test-deps.package.json](test-deps.package.json)及[lock](test-deps.package-lock.json)。完整来源与checksum见[sources.json](sources.json)。

已在新的临时目录实际执行上述复跑脚本，四组共230项再次通过，上游跟踪源码检查通过。见[复跑结果](reproduce.log)和[复跑版本记录](reproduced-revision.json)；表中的四组日志已更新为此次干净复跑的输出。
