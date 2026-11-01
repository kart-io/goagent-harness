# Sandbox 补充调研验证报告

日期：2026-11-01。实现基线：`e8c7ebf`。本轮修改文档、调研探针和验证脚本的可选`--probe`入口，没有新增生产Provider生命周期接口或改变运行时默认值。

## 方法与环境

复用原先的私有Docker控制服务fixture：固定server/guest digest与execd digest、独立SQLite、bridge、随机本机端口和临时密钥。完整版本与镜像见[首次验收](../2026-11-01-sandbox/REPORT.md)，当日资料和固定源码checksum见[sources.json](sources.json)。

本轮在同一真实沙盒中进行原生SDK生命周期探针和小样本端到端测量，不将模拟HTTP或文档声明当成部署实证。Go1.25.0；每组先做一次不计入的warm-up；文件操作8个串行样本、命令4个。镜像已缓存，P50取排序后较低中位、P95取nearest-rank；这里n很小，P95等于样本最大值，不是生产P95估计。

实际Docker inspect选取字段：cpu=500000000 NanoCpus（0.5CPU）、memory=268435456（256MiB）、PidsLimit=256、Runtime=runc、NetworkMode=bridge、ReadonlyRootfs=false。PID限制来自本轮fixture，不能当成Provider默认值。未运行OOM、fork-bomb、网络隔离或容器逃逸测试；inspect只证明配置字段，不证明所有负载行为。

## 契约探针

| 探针 | 实际结果 | 能证明什么 |
| --- | --- | --- |
| TTL=30s | New接受，Create HTTP422，Environment=nil | 本地允许范围与固定服务最少60秒不一致；没有本次返回的分配 |
| 原生RenewExpiration | 返回的expiresAt晚于原值 | 上游SDK+固定Docker服务支持基础续期；不是Harness自动续期 |
| 原生PauseSandbox/ResumeSandbox | 状态Paused→Running，文件内容保留 | 基础暂停恢复可用；没有验证与活动Session/Close并发 |
| 实际runtime/resource字段 | runc、bridge、0.5CPU、256MiB、PID256 | 本轮是普通Docker运行时，不是gVisor/Kata/VM验收 |
| Destroy | API确认，独立Docker查询实例容器不存在 | 本轮实例回收通过，不以返回nil单独充当物理清理证据 |

未执行快照创建/恢复。快照、网络策略、Kubernetes等能力仅按固定官方源码记录，不能标成实测。TTL差距列入路线图S0，本次只修正文档的有效配置说明，未修改New。

## 小样本测量

单位：耗时ms；HTTP统计包括客户端经控制代理的请求，不包含服务端内部转发。计数在warm-up完成后开始，表中的总数只包含计时样本。创建就绪约347.322ms，为单次热镜像观察，不是冷启动SLA。

| 操作 | PollInterval | 样本数 | P50(ms) | 样本P95(ms) | HTTP总请求 | 平均请求/次 |
| --- | --- | --- | --- | --- | --- | --- |
| command_python_print | 默认50ms | 4 | 263.636 | 266.657 | 64 | 16.00 |
| read_17_bytes | 默认50ms | 8 | 134.649 | 142.352 | 72 | 9.00 |
| resolve | 默认50ms | 8 | 134.082 | 165.244 | 72 | 9.00 |
| stat | 默认50ms | 8 | 133.316 | 137.279 | 72 | 9.00 |
| write_17_bytes | 默认50ms | 8 | 139.327 | 204.049 | 74 | 9.25 |
| command_python_print | 对照5ms | 4 | 151.485 | 155.569 | 102 | 25.50 |
| read_17_bytes | 对照5ms | 8 | 101.037 | 109.083 | 146 | 18.25 |

5ms对照使用同一实例的借用连接，权限/文件根/镜像未变，串行执行。它不是随机化或多机器实验；温度、缓存和调度都可能影响数值。数据表明本环境减少等待可以降低部分延迟，同时增加HTTP负载，不能仅据此修改默认值。

工程推断：逐文件辅助程序的往返成本足以影响多文件工具，下一步应测真实项目任务，比较有界批量FS/传输或文件服务。不能直接用这些时间推算并发容量、费用、所有文件大小的性能或安全等级。

## 原有链路与清理

真实生命周期/文件系统与完整preset测试再次通过，编码工具和workflow案例也再次通过；原始输出见[real-test.log](real-test.log)、[coding](example-coding.log)、[workflow](example-workflow.log)。

[cleanup.json](cleanup.json)记录三个原测试实例及新增研究实例的独立Docker删除确认。临时控制服务finally删除。两个示例也使用Registry.Close关闭各自实例；未把未记录的例子ID冒充独立inspect证据。

## 复跑

```sh
python3 docs/evaluation/2026-11-01-sandbox-research/reproduce.py --output /tmp/harness-sandbox-research-results
```

需要Docker、Python3、Go1.25、固定镜像及Docker socket访问。脚本创建本轮独立服务、运行既有验证与额外可信Go探针，清理本轮实例。`scripts/verify-sandbox.py --probe /path/to/probe.go`会在同一临时fixture中执行宿主提供的Go代码；这是可信维护者的调研工具，不是模型或网络用户的执行入口。

探针源码保存在[probe.go.txt](probe.go.txt)，复制到临时.go文件后运行。原始数据见[research-probe.json](research-probe.json)，环境声明与局限随结果保存，密钥不归档。

## 文档与代码核对

新增能力调研、路线图、API参考和场景指南；现有接入说明改为明确上游最短TTL60秒及本地校验差距。新增文档的本地链接、源码引用和配置数字核对；验证脚本默认行为保留，额外探针只在显式--probe时执行。E2B/Daytona/gVisor/Kata/Firecracker没有部署验证。

文档中的reproduce.py入口已再次实际执行成功，见[复跑日志](reproduce.log)、[独立复跑数据](reproduced-probe.json)与[复跑清理](reproduced-cleanup.json)。上表保留第一次带HTTP计数的探针数据，不把两轮结果混算；各轮小样本会自然波动。脚本不在外层强制杀死fixture进程，具体命令和探针各自有期限，让fixture保留finally清理机会。文档/脚本检查见[verification.log](verification.log)。
