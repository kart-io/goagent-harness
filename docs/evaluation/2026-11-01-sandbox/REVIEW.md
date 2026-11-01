# 单次独立审阅与处理

独立审阅：gpt-6-astra/high，fresh context，只读检查 `11520f4..7cfdd3e`，未派生其他审阅者。审阅者独立执行 sandbox/preset/adapter race 测试，并用临时 overlay 复现额外回归；没有修改工作树。结论为 With fixes：0 Critical、5 Important、0 Minor。

执行者按实际影响复核，五项均维持 Important，并在一次修复过程中处理。没有二次审阅；关闭依据是失败回归、修复后完整测试、真实服务复验。

| 发现 | 修复 | 证明 |
| --- | --- | --- |
| 无 workspace 的中间子 Agent 丢失祖先网络/沙箱上限 | 先继承全部祖先限制，再与当前 workspace 取交集；分配前拒绝 | `TestReviewGrandchildCannotLoseAncestorCeilings`，分别测试 sandbox/network |
| 安全路径替换为 .git/config 链接后可写入 | coding 的最底层再次校验；可选 MutationProtector 由 Session 保留，helper 实际变更检查保护条目名；变更拒绝别名重定向 | `TestReviewCodingRejectsLinkSwapBeforeMutation`，4个Resolve阶段与最终write；Session能力与生命周期测试 |
| 编码超时吞掉 UnconfirmedError | 未确认结果保持失败、完整 error chain 和身份，只有已确认的超时分类成普通 timed_out | `TestReviewCodingMustPreserveUnconfirmedTimeout`、`TestCodingAdapterFailedInterruptPreservesIdentity`；后者用真实SDK HTTP状态/中断路径 |
| HTTP408 启动被当成确定拒绝 | 408 标记 ErrUnconfirmed，与创建一致；Environment 对未确认结果拒绝后续操作 | `TestReviewHTTP408IsUnknownCommandOutcome`，未确认环境拒绝新命令测试 |
| 未核对实际服务和 SDK 版本 | SDK.Version构造检查，server /version 在 Create/Connect 分配前核对；execd 要求显式可信部署digest pin | `TestReviewRejectsWrongDeployedServerVersion`、`TestRuntimeReleaseAndDeploymentPinGates`、真实固定运行时 |

首次失败见 [review-red.log](review-red.log)；强化回归对旧生产代码的失败见 [review-strengthened-red.log](review-strengthened-red.log)。最终全量通过见 [combined-race.log](combined-race.log) 和 [real-test.log](real-test.log)。根修复 `5e7d5be`、可选模块修复 `5d5b07f`。

## 审阅者不裁定的边界及执行者决定

1. 直接命令/外部编辑器的 CAS：保留明确排除，辅助程序锁不能约束绕过接口的写者；代价是宿主需重新读取。
2. 脱离进程组的后代终止：保留明确排除，owned实例整体销毁为最后边界；borrowed所有者负责残留。
3. network=false、Kubernetes、微虚机：首期不支持，网络关闭在分配前拒绝；需要独立适配与验收。
4. 自动TTL续期与未保留实例的持久恢复：由宿主管理TTL、Retain/Connect；过期/新实例不能伪装恢复成功。
5. 任意不遵守context的Provider/transport：不承诺强停不合作的Go实现；宿主必须选择符合契约的实现。

execd Ping 不提供构建版本，必须由宿主实际部署声明的固定镜像，Options pin不能远程证明部署真实性。其他后端如需相同的最终变更保护，应实现可选MutationProtector；本轮没有修改GoAgent核心接口。

没有延期小项。全部裁定及代价原文见 [执行ledger](EXECUTION_LEDGER.md)。
