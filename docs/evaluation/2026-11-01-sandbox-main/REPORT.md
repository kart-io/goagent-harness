# Sandbox 主线整合验收

日期：2026-11-01。合并目标：main a3a7e91 + Provider 分支 e50af16。根据用户最新要求，最终源码树去除供应商适配器、独立 Go 模块、SDK、专属案例、部署验证脚本及其文档/实测产物；既有 Git 历史保留。当前只有根 Go 模块，不再交付远端服务实现。

## 保留与整合

主线 DSH native/docker/local、Policy、Controller、单次批准、技能边界及原有安全测试保留。合入供应商无关的 Provider/Environment、Registry/Session、实例所有权与可重试清理，以及 Pi 固定源码调研。GoAgent 依赖由 v0.23.0 更新为分支使用的 v0.32.2，golang.org/x/sys 保留主线 v0.41.0。

managed 命令必须实现 PolicyExecutor.CheckPolicy/RunWithPolicy，装配时检查默认策略和最高可批准策略，调用时传入权限控制器核验的有效 Policy。只凭 Isolated/Container/Network 声明不能开放命令。无策略接口仅允许 sandbox=read-only,maxSandbox=read-only 的文件视图，不提供命令或 linter。附加宿主目录与环境变量映射一律在分配前拒绝；没有 workspace 的中间子 Agent 仍保留父级上限。

Host.Root 是本地授权域，远端 Root 是后端身份与文件空间，二者不自动挂载。具体后端仍须实际证明路径、网络、条件写及进程限制；本轮没有远端供应商运行验收，也不把受控 Provider fixture 说成产品后端。

## 执行结果

环境：Darwin arm64，Go1.25.0，GOWORK=off，unset GOROOT。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 全量 go test -race -count=1 -json ./... | 退出0，5个测试包，167条测试/子测试通过记录 | race.jsonl |
| go build ./... | 退出0，包含全部3个原有案例 | build.log |
| go vet ./... | 退出0 | vet.log |
| native 单次审批案例 -approve | first失败、approved成功、last再次失败 | approval.log |
| 供应商名称、导入、目录与部署脚本扫描 | 源码树无残留；只有根go.mod | 最终提交文件树 |

4项跳过：TestDockerExecutorWorkspace、TestLinuxProcRootCannotEscape、TestDockerReal、TestDockerUnixSocketReal。当前没有配置 Docker 测试镜像，Linux PID namespace 测试不适用于 Darwin；这些不是平台通过证据。coding-agent/plan-agent 已构建，未追加付费模型运行。Pi 的230项证据是既有分支调研产物，本轮保留，未重跑上游 Node 测试。

合并回归覆盖无结构化强制的后端拒绝、失败清理、分配前宿主上限、未映射目录/env拒绝、一次批准与失效、会话权限变更传入执行器、父级无workspace中继不丢约束。RED/GREEN实施摘要见 IMPLEMENTATION.md。

## 审核与交付

独立审核发现并修复未知结果被默认 durable 策略重放的问题：commandTool（含命令、文件修改与 Git）声明 MaxAttempts=1，权限/schema 包装完整透传策略。真实 Preset、Dispatcher、重开 FileStore 同 journal/CallID 验证 run_command/git_commit/write_file 不重复副作用，首次 ErrUnconfirmed、恢复 ErrOperationAttention。最终全量 race/build/vet 与 native 案例通过。独立限定复审确认 Important、Minor 全部关闭，可合并；详见 REVIEW.md。合入并推送 main，既有 Git 历史不改写。
