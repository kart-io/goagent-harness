# Sandbox 能力、选型与生产接入调研

日期：2026-11-01。当前 Harness 基线 `e8c7ebf`；OpenSandbox 固定 `release-1.2.0 / 62a64f0b48cdd6b410f36e53c74dc7559c87719a`。其他项目的维护状态通过当日官方仓库与文档核对，[来源清单](../evaluation/2026-11-01-sandbox-research/sources.json)保存了仓库 revision、归档状态和固定源码摘要。

本文把三个层次分开：**供应商有接口、Harness有适配、实际部署已验证**。不能把第一项直接填成后两项。后续接口提案见[路线图](../design/SANDBOX_ROADMAP.md)，当前 API 见[参考](../reference/SANDBOX_API.md)。

## 调研结论

1. 继续以 OpenSandbox 作为首个可替换后端，优先补齐运行时声明、生命周期与生产资源治理，暂不同时引入多个 SDK。
2. `Isolated=true` 目前只说明适配器声明有隔离；本机 inspect 实际为 runc/bridge，不是 gVisor、Kata 或 microVM 的验证结果。
3. 网络策略、续期、暂停、快照是上游能力，当前 Harness 不提供这些控制接口。续期与暂停恢复已用原生 SDK 真测，但这不等于已通过 Registry/Session 接入。
4. 小文件操作有明显的逐次上传/执行/轮询开销。优先设计批量传输或受控文件服务，不能只把默认轮询调快。
5. 多 Agent 默认独立实例，工作流只负责控制流；跨实例共享应传递有校验的产物，不共享可写工作区。
6. 生产前应补租户授权、配额、持久资源记录、孤儿回收和退出排空；这些能力目前不由内存 Registry 自动提供。

以上为基于证据的工程建议，不是所有部署环境的性能或隔离认证。

## 可选方案比较

| 方案 | 定位与已核实事实 | 对 Go/Harness 的影响 | 本轮结论 |
| --- | --- | --- | --- |
| OpenSandbox | Apache-2.0；固定源码含 Docker/Kubernetes、网络策略、续期、暂停与快照实现 | 已有官方 Go SDK 和独立适配模块 | 优先深化现有接入；本轮只真测固定 Docker 配置 |
| E2B Runtime | 官方 runtime 当前为 Apache-2.0，使用 Firecracker；原 infra 地址跳转 runtime | 用户文档主要展示 TS/Python；Go 后端实现不等于已验证的客户端 SDK，可另行评估协议或独立桥接 | 作为 microVM/云端候选；本轮没有部署和客户端适配验证 |
| Daytona | 原 daytonaio/daytona 仓库已归档，README称2026年6月核心迁入私有代码库；daytona/clients仍公开，含官方Go SDK | 可以研究服务适配，不能把活跃客户端误当成活跃开源服务端 | 不列为持续维护的开源后端首选；商业服务另行评估 |
| gVisor | 用户态内核/OCI runtime，增加 syscall 隔离层 | 可由 OpenSandbox 服务端选择 runtime profile，而不是新增 Agent 执行循环 | 可作为现有服务的隔离升级；需目标Linux环境兼容性实测 |
| Kata / Firecracker | Kata 是VM容器运行时路径；Firecracker 是依赖KVM的microVM VMM | 仍需镜像、节点、网络、调度和运维；不能直接实现一个HTTP Provider就获得完整平台 | 高隔离候选；不在本机Docker验收中宣称已支持 |

依据：[OpenSandbox固定配置](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/configuration.md)、[E2B runtime](https://github.com/e2b-dev/runtime)、[E2B客户端入口](https://github.com/e2b-dev/E2B)、[Daytona归档说明](https://github.com/daytonaio/daytona)、[Daytona客户端许可范围](https://github.com/daytona/clients#license)、[gVisor机制](https://gvisor.dev/docs/architecture_guide/intro/)、[Kata官方说明](https://github.com/kata-containers/kata-containers)、[Firecracker宿主要求](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)。这里记录项目声明和仓库许可标识，不推断历史版本或全部依赖采用同一许可。

### 维护状态会改变选型

当日 GitHub API 显示 daytonaio/daytona 的 archived=true；其当前根目录没有 LICENSE 文件，不能凭旧经验断言当前服务端许可。daytona/clients 对 SDK/API客户端标注 Apache-2.0，对CLI标注AGPL-3.0。官方Go文档可访问并不撤销核心代码迁移的声明。[Go SDK文档](https://www.daytona.io/docs/en/go-sdk/)、[固定归档README](https://github.com/daytonaio/daytona/blob/ec4c21b2d597091ac09ecc278f3bcc172575a987/README.md)。

E2B 的官方公开 runtime 和当前文档值得重新评估；不能继续使用“只有云服务、无法自托管”的旧判断。其用户级SDK文档主要列TS/Python，Go库可用性、API兼容和实际自托管成本仍需独立验证。[Runtime README](https://github.com/e2b-dev/runtime)、[官方SDK入口](https://github.com/e2b-dev/E2B#what-is-e2b)。

## 能力矩阵

状态：**已有**=当前代码和既有验收；**原生真测**=本轮绕过Harness控制接口，用官方SDK验证；**源码**=固定上游源码存在；**提案**=尚未交付。

| 能力 | 上游证据 | 当前Harness | 下一步验收 |
| --- | --- | --- | --- |
| 创建/连接/删除 | lifecycle SDK | 已有，受管所有权与失败重试 | 扩展服务重启、TTL竞争与错误矩阵 |
| TTL续期 | RenewExpiration；原生真测到期时间延长 | 没有公开续期接口/自动心跳 | 与Close并发、最大存活期、恢复后的租约归属 |
| 暂停/恢复 | Docker pause/resume；原生真测且文件保留 | 没有公开状态转换接口 | 先停准入/排空活动命令，再暂停；失败可对账 |
| 快照 | DockerSnapshotRuntime使用docker commit，SDK有Snapshot API | Request只有Image，无SnapshotID | 分清Docker文件系统快照和VM内存快照；新实例的新身份 |
| 网络默认拒绝/白名单 | NetworkPolicy、egress sidecar、服务端profile | 只接收Network=true；false分配前拒绝 | 域名、直连IP、IPv6、UDP、元数据地址、跨沙盒、策略故障 |
| 更强隔离 | 服务端secure_runtime配置 | Isolated声明，无runtime profile实证字段 | 在目标部署验证runsc/Kata/VM和实际生效配置 |
| CPU/内存 | ResourceLimits传给服务端，inspect已核对本机字段 | 有供应商资源参数，没有业务任务级配额 | OOM/CPU/PID负载行为、并发任务额度、磁盘限额 |
| 远端FS/CAS/保护路径 | Harness自有Python辅助程序 | 已有，合作写者CAS、.git最终保护 | 更大目录、并发rename、受限用户和故障注入 |
| 大文件/产物同步 | 上游文件/volume接口 | 单文件有界读写，无同步服务 | 总字节预算、校验和、路径过滤、重复上传与失败清理 |
| 运行日志/事件 | 原生SDK与命令日志 | Run返回有界合并输出，无公开事件流 | 背压、断线、重复事件、脱敏和恢复游标 |
| 服务端端口/预览 | GetEndpoint/代理入口 | 适配器只在内部取execd端点 | 单独设计访问授权、生命周期和公开URL，不混用Agent入口 |
| PTY/交互/浏览器 | 上游项目有不同数据面能力 | 当前Executor没有PTY/浏览器协议 | 可选工具或子模块；不能伪装成普通命令Run |
| Credential Vault | 上游独立凭证代理设计 | 没有Request字段或能力适配 | 密钥不入模型/日志，吊销、短期授权和出站策略 |
| 预热池/快照分叉 | 上游有不同runtime的池/模板能力 | 默认每次Open新建，无池和自动复用 | 跨任务污染、独占租约、版本匹配、释放与过期 |
| 多租户持久治理 | 需服务端与宿主业务共同完成 | Namespace只是作用域，Registry内存态 | 身份授权、持久记录、配额、孤儿回收与审计 |

源码依据：[SDK lifecycle](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/sdks/sandbox/go/lifecycle.go)、[API schema](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/opensandbox_server/api/schema.py)、[Docker生命周期](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/opensandbox_server/services/docker/docker_service.py)、[Docker快照实现](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/opensandbox_server/services/docker/snapshot_runtime.py)。真实证据见[本轮报告](../evaluation/2026-11-01-sandbox-research/REPORT.md)。

## 网络与隔离：必须核实的实际差距

固定上游Docker默认network_mode是host，而既有验收脚本显式设置bridge；两者不能混为一谈。当前适配器不会下发NetworkPolicy，服务端配置egress镜像本身不足以证明本适配器实例已附加策略sidecar；固定源码在请求存在network_policy时才进入对应创建分支。网络可用的布尔能力也不表示“只允许互联网而禁止内网”。[配置](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/configuration.md)、[创建分支](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/opensandbox_server/services/docker/docker_service.py)。

上游DNS-only与dns+nft执行机制不同；DNS允许/拒绝不能自动推导直连IP、IPv6或UDP的覆盖。域名规则和端口规则也有组合限制。下一阶段必须先定义要隔离的网络范围，再验证所选模式，不把 `network:false` 简单映射成未验证的标签。[Egress架构](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/docs/architecture/network/egress.md)。

固定服务的secure_runtime是服务级配置，不是模型逐请求选择；同一集群多个强隔离profile需要分开寻址。gVisor是用户态内核路径，不能统称硬件VM。宿主应以可信Profile绑定Provider、镜像与资源约束，而不是让模型提供runtime字符串。[配置约束](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/configuration.md#secure_runtime)、[gVisor说明](https://gvisor.dev/docs/architecture_guide/intro/)。

## 身份、幂等与恢复

Registry Namespace/Scope 不执行租户认证。相同Scope重复Open默认创建不同ID；随机owner token用于未知创建对账，不是供应商幂等键。Registry和Resolver映射也不会自动持久化。当前Resolver回调签名不带context；它必须及时返回，Registry取消不能强行打断一个阻塞回调。宿主应在调用前写入创建意图，记录任务、租户、Scope、owner/实例身份、保留策略与清理状态；未知结果禁止直接再创建。[现有代码](../../sandbox/registry.go)。

Retain只是不删除正在保留的实例，不是磁盘/内存快照。借用Connect、恢复同实例、从快照创建新实例、重新执行业务任务是四种不同动作。Docker commit不能承诺保留运行中进程和内存；从快照产生的新ID不能直接沿用旧Manifest假装同一执行环境。[所有权](../../sandbox/session.go)、[快照源码](https://github.com/opensandbox-group/OpenSandbox/blob/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server/opensandbox_server/services/docker/snapshot_runtime.py)。

现有Manifest记录ID、Provider版本与能力，但宿主Provider的资源、网络执行profile等未形成完整可核对的运行时证明。后续需区分稳定执行配置与可变化的租约到期时间，避免把每次续期误判为执行身份变化。

## 性能与规模

本轮固定Docker、本地代理、热镜像、串行小样本：默认ReadFile(17字节)中位约135ms；每次仍需请求上传、Python辅助命令、状态/日志与请求文件清理。普通Run还先远端Resolve目录，再执行命令。5ms轮询的读取中位约101ms，但每次平均HTTP请求约从9增至18.25。完整数值和样本数见[报告](../evaluation/2026-11-01-sandbox-research/REPORT.md)。

工程推断：大量小文件检索/编辑比单次长计算更容易放大往返成本。应先测真实仓库的glob/grep/edit，再评估有界批量FS协议、缓存的失效契约或受控文件服务。任何改动都必须保留版本条件、保护路径和取消后的未知结果，不能以速度为由取消这些语义。小样本不推导生产P95、并发上限或冷启动SLA。

## 落地顺序

先修契约差距与生产资源记录，再接生命周期可选能力，随后做产物流转、批量FS和网络profile，最后在目标Linux/Kubernetes环境验证强隔离与其他供应商。具体任务编号、依赖和验收标准见[路线图](../design/SANDBOX_ROADMAP.md)。本次新增资料和探针，不把上游SDK真测能力直接升级成Harness已支持功能。
