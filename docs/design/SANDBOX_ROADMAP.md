# Sandbox 后续路线图

状态：以下为提案；供应商无关生命周期和主线策略已合并，实际结果见 [本轮验收](../evaluation/2026-11-01-sandbox-main/REPORT.md)。不将未来功能列为当前 API。

| 阶段 | 内容 | 先决条件与验收 |
| --- | --- | --- |
| S0 策略能力契约 | 对齐 managed 后端和主线 structured Policy | 明确路径映射和逐次强制，不能只检查 Isolated/Container |
| S1 资源台账与配额 | 创建意图、租约、去重、孤儿清理 | 未知创建、进程中断、重复关闭、宿主权限及并发测试 |
| S2 续期/暂停/恢复 | 独立可选生命周期扩展 | 与 Close/取消竞争、借用身份、未知结果核对 |
| S3 产物与批量文件 | 宿主传输模块或可选 FS 能力 | 总量、hash、路径/CAS、原子写和真实性能验证 |
| S4 远端后端 | 宿主适配独立实现 | 先通过 `sandboxtest` 契约和 `Faults` 故障注入，再验证目标部署的策略、运行时、网络、资源和故障。`sshsandbox` 是第一个内置的远端 Provider，在真实 OpenSSH 上通过契约和断线故障用例。自托管的 OpenSandbox 已作为测试 Provider 通过契约（见下）；下一步用 Daytona 做第一个云厂商冒烟 |
| S5 快照/模板/池 | 带实例代际和清理责任的资源复用 | 防止跨任务污染，不把新实例标成原环境恢复 |

OpenSandbox 用于验证真实 HTTP 远端，不需要云账号。启动服务后设置 `GOAGENT_OPENSANDBOX_URL` 运行 `TestOpenSandbox*`。它发现的差异已写进 `integration/opensandbox_provider_test.go`：实例 `Running` 时执行服务可能还连不上，所以 `Create` 要等就绪；客户端放弃 `Create` 后实例可能才出现，这时 `FindByScope` 查不到，因为后端没有幂等键，泄漏只能靠实例 TTL 兜底。云厂商后端要在 S4 单独确认是否提供幂等创建。

真实列表的收敛时间、限流和配额、休眠恢复、跨地域长连接、凭据过期和成本，只有真实云账号才能验证。

所有远端功能需单独设计和验收，不在根模块强制绑定供应商 SDK。Pi 的环境协议、持久子任务、动态工具发现等采纳顺序见 [Pi 计划](PI_AGENT_ADOPTION_PLAN.md)。
