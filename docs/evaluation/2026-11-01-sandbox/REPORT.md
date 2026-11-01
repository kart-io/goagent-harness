# Sandbox 实施与验证报告

日期：2026-11-01。分支：`feat/sandbox-provider-20261101`。基线：Harness `11520f4`，GoAgent 从 v0.23.0 升至 v0.32.2；升级前后既有全量 race 测试均通过。工作在独立 worktree，仅本地提交，未推送、合并或发布。

## 功能交付

- 通用 Provider/Environment/Registry/Session，创建与重连、所有权、Retain、受管操作取消、等待真实返回、失败关闭重试与迟到资源回收。
- Preset 的 sandbox executor/provider 配置、远端工作区注入、能力检查、只读工具收窄、父子网络/权限检查、完整 Scope 与真实 SandboxID Manifest。
- 独立 OpenSandbox 模块：低层 SDK 生命周期、同源 server proxy、无自动重试、响应限制、异步命令、独立中断确认、完整 rooted FileSystem。
- 辅助程序条件写、SHA256、flock、no-follow 目录访问、原子替换/fsync、二进制文件、Walk 分页及 SkipDir/SkipAll。
- 编码工具、固定 workflow 两个可运行案例、完整使用/设计文档与真实 Docker 复跑脚本。

## 固定验证组合

| 项 | 值 |
| --- | --- |
| Go | go1.25.0；命令清除旧 GOROOT |
| GoAgent | v0.32.2，发布模块 |
| SDK | github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.2.0 |
| 上游源码 | release-1.2.0，62a64f0b48cdd6b410f36e53c74dc7559c87719a |
| server/验证用 guest | opensandbox/server@sha256:25bebdcf1cf7724eb923faecbdbbf82d4038c6ab35c736151574726d14006f30 |
| execd | opensandbox/execd@sha256:c75836c46ef4302afccec182d5d7339eb2f905f51e3d73b7aa17bb76c832526c |
| guest Python | 3.10.22 |
| 后端 | Docker bridge；本机临时控制服务、独立 SQLite、server proxy |

## 已执行的验证

根模块：`GOWORK=off go test -race -count=1 ./...`、`go vet ./...`、golangci-lint v2.6.2 `run --no-config ./...` 均通过。可选模块：临时 workspace 下完整 `go test -race -count=1 ./...`（包括案例编译）、`go vet ./...`、相同 lint 均通过。根依赖不含 OpenSandbox SDK；嵌套模块需要单独验收。原始 race 输出见 [root-race.log](root-race.log)、[adapter-race.log](adapter-race.log)。

`python3 scripts/verify-sandbox.py --output /tmp/harness-sandbox-acceptance3`：

1. 原生 SDK HTTP 与真实 Docker：创建、就绪、文件/命令一致、超过单参数限制的大二进制文件、条件冲突、特殊 Argv、非零退出、链接越界、取消后实际 PID 消失、输出上限中断、断开与重连、独立环境均通过。
2. 完整 preset.Load → Build → Dispatcher 的远端 write_file/read_file 通过。模板包含 permissions/base=coding，命令仍要求宿主审批。
3. 编码工具案例通过，workflow 输出 `HELLO WORKFLOW`。见 [coding 输出](example-coding.log)、[workflow 输出](example-workflow.log)。
4. 三个已记录沙盒在 Destroy 后通过独立 Docker 查询确认容器不存在，见 [cleanup.json](cleanup.json)。临时控制服务在 finally 删除，两个案例也由 Registry 关闭各自环境。

真实测试输出见 [real-test.log](real-test.log)。测试不会把模拟 HTTP/本地 Python 当成真实服务证据：另有控制 HTTP 测试覆盖协议失败与本地真实 Python 文件测试覆盖并发 CAS/遍历边界。

## RED → GREEN 发现与修正

- 通用 API、preset Host、可选后端与文件系统均先执行缺少接口的测试，再实现。
- 有界关闭、网络不匹配、嵌套路径 Resolve、元数据标签约束有行为失败后通过的记录。
- 真实服务拒绝原始 scope/root/image 标签：改为稳定摘要及标签安全版本，重连另外验证实际镜像。
- SDK 上传 mode 使用八进制数字形式的整数；通过 SDK.OctalMode 转换。
- 生命周期 Running 比 execd 就绪早：新增只读 Ping 等待，启动过程整体限时，业务命令与创建不自动重试。
- 首个完整 preset 探针因漏配 permissions 被现有默认 Ask 拒绝；修正示例为显式 base=coding，没有放宽宿主默认权限。
- 根模块未发布依赖导致首次临时 tidy 配置失败、两次 shell 使用错误工作目录不算功能 RED；均未修改原工作区。

## 约束与责任

首期仅验证 Docker、Linux python3/sleep infinity、Network=true、server proxy。网络关闭请求预分配拒绝；没有验证任意服务部署、Kubernetes、微虚机或出站隔离。远端工作目录不自动挂载宿主目录。文件辅助程序 CAS 不协调直接命令写入，容器内命令也不被 FileSystem 根边界限制。取消/落盘确认失败时文件可能已经变化，需要读取确认。进程中断不保证脱离进程组的所有后代清理。

创建响应未知时保留 owner token；空查询不证明未创建，Registry 清理可重试，宿主需对账并以 TTL 兜底。Retain/borrowed 的实例由宿主最终销毁；Manifest 同一性依赖保留并重连真实实例。TTL 不自动续期。自定义 HTTP transport 必须遵守 context。

可选模块当前引用本轮根提交，发布前通过文档的临时 workspace 使用，发布时须协调根模块和子模块版本。未自动修改其他仓库或 GoAgent 内核。

## 最终独立审阅

待执行一次全分支独立审阅；发现、裁定和复验结果在此更新。过程 ledger 在结束前归档。
