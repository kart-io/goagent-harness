# Sandbox 接入设计与功能契约

状态：实现与验证中。2026-11-01。Harness使用GoAgent v0.32.2；不修改GoAgent执行内核。首个可选后端为OpenSandbox，固定release-1.2.0（62a64f0b48cdd6b410f36e53c74dc7559c87719a），server/SDK/execd成套验证。

## 职责与功能

| 位置 | 交付 |
| --- | --- |
| GoAgent harness/env | 复用Executor、FileSystem、Command、Version与现有错误，不引入供应商SDK |
| Harness sandbox | Provider/Environment、宿主Registry、创建/连接、所有权、操作取消和资源释放 |
| Harness preset | executor=sandbox、provider选择、真实能力检查、构造失败回收、子Agent收窄 |
| sandbox/opensandbox（独立Go模块） | Go SDK、命令、完整远端工作区、取消确认、会话创建/连接/销毁 |
| 可选模块examples | 可运行的工作区/编码任务及固定任务案例，独立编译和真实服务验证 |

默认local/docker行为保留。已有Sandbox枚举仍是read-only/workspace-write/danger-full-access权限模式，后端类型使用Executor/Provider表达。SDK、控制服务地址和密钥只由宿主构造Provider，不进入模型参数或YAML秘密明文。

## 抽象与资源所有权

新增Provider.Create/Connect，返回Environment；Environment提供ID、Root、FileSystem、Executor、隔离声明、Close和Destroy。Close断开客户端，Destroy销毁后端实例，二者不能混用。

Registry由宿主注入preset.Host，按Provider名选择实现。Request包含Scope、Image、Network；Scope由装配路径产生，Registry增加宿主Namespace。默认每次创建独立实例；宿主Resolver可为同Scope恢复提供已有ID与Retain选项。沙盒ID不由模型/YAML指定。

Registry同时跟踪尚未返回的Open和所有未关闭Session，提供可重试Close供宿主在装配失败后统一回收。远端FS需实现ContextResolver以让无context参数的Resolve参与取消。Registry返回受管Session：新建实例默认owned，Close先关闭准入并取消活动操作，等待操作实际返回，再Destroy并Close；连接已有实例为borrowed，仅Close；显式Retain可保留新建实例以便恢复。Close有context并可在超时后重试，不以一次超时永久标记资源已回收。Backend方法必须遵守context；无法强停任意不配合的Go实现。

Session包裹Executor与FileSystem。操作使用调用context与会话生命周期共同控制，关闭后新操作拒绝；取消FileSystem操作同样纳管。Resolve虽没有context参数，仍受会话生命周期及后端操作期限约束。

Registry.Open即使在Create迟到或验证失败时也保留非空会话的清理责任；调用者必须先接管非空Session再处理错误。Preset登记OnClose后再继续装配。未取得后端ID的未知创建结果不能自动重试，使用TTL与服务端元数据排查。

## Preset配置与权限

```yaml
use: workspace
config:
  executor: sandbox
  provider: opensandbox
  image: your-python3-image
  sandbox: workspace-write
  network: true
  commandTimeout: 30s
  maxOutputBytes: 1048576
```

具体源文件结构沿用preset现有格式；上面仅组件片段。Host.Root仍是本地配置/信任身份，实际工具工作区来自Environment.Root；不能把本地Root假装挂载到了远端。命令与文件必须使用同一个Environment。

新增能力仍通过Needs/Trust/Limits校验。provider进入executor授权标识，image和network沿用现有上限。能力不满足立即失败并回收已创建资源，不降级本地执行。read-only包裹远端FS；没有ReadOnlyRoot能力时禁用命令工具。workspace-write要求真正隔离的环境，而不把Remote=true当隔离证明。

子Agent默认独立环境，继承父级沙箱与网络上限，扩权在远端资源创建前拒绝。首期不支持跨Scope共享写会话；同Scope连接用于恢复。Manifest包含真实Environment ID、Root、Provider版本和能力，不能靠省略随机ID假装新建空环境等价于恢复。恢复者需Retain并用同Namespace/Scope连接原实例，再进行PinManifest核对。

## OpenSandbox首期边界

- 独立模块导入官方Go SDK的实际模块路径github.com/alibaba/OpenSandbox/sdks/sandbox/go，根模块不直接依赖SDK。
- 首期明确支持已验证的Docker服务后端、Linux沙盒镜像。镜像须有python3和sleep infinity，初始化实际探测，缺少即失败。已有server镜像中python3可用于本地验证；业务镜像由宿主指定。
- 工作区默认/workspace/goagent，根目录由可信Provider配置指定；不自动上传本地项目。文件上传可用FS，产物由调用者读回。
- 首期只支持网络可用配置；network=false在创建前明确拒绝，不声明未经验证的出站阻断。网络策略/微VM/Kubernetes支持需单独验收。
- Create设置TTL（默认10分钟）和可信版本/Scope元数据（Root/Scope/Image使用无padding base32 SHA256以适配63字符标签，实际镜像额外核对）；Connect核对元数据与配置，不将任意外部沙盒伪装为本适配器实例。
- Native Argv不拼接shell；Dir必须在工作区。使用异步命令ID及状态/日志接口，非零退出码返回Result,nil；启动/传输/取消失败返回error。
- 命令输出有上限，RPC响应也有边界。达到上限或context取消时使用独立清理context发送Interrupt，并查询停止状态；不能确认时返回带sandbox/command身份的可识别未确认错误，不自动重新执行。
- 进程状态验证不承诺逃离进程组的所有后代都被清理；owned实例在最终关闭时销毁，borrowed实例由其所有者管理。

## 远端FileSystem

通过固定嵌入的python3辅助程序在同一沙盒内执行文件操作，参数经Argv/JSON传递，不插入shell脚本。提供Resolve/Stat/ReadFile/WriteFile/Remove/Walk，内容Version使用SHA-256。文件、遍历结果和RPC有独立大小上限。

路径先规范并限定在配置根内；操作使用目录fd和O_NOFOLLOW遍历，拒绝危险链接/替换。Resolve可解析指向工作区内部的符号链接，拒绝越界；Walk按词法顺序且不跟随符号链接，Go侧保留fs.SkipDir/SkipAll语义。Remove删除链接本身，不跟随目标。

WriteIntent.CreateIfAbsent/IfVersion与写入通过沙盒内flock协调所有辅助程序写者，原子替换、fsync并返回内容版本；空IfVersion不构成条件。协调仅覆盖遵守协议的辅助程序操作，不覆盖任意命令或外部编辑器直接写入，不声称跨任意写者CAS。路径竞态、不支持类型、超限和持久化确认失败必须明确返回，不能伪报成功。取消/中断时可能已发生写入，应重新读取确认。

## 验收

1. 核心接口/错误不复制；根模块与GoAgent v0.32.2组合race基线通过。
2. Provider替换、owned/borrowed/Retain、关闭竞态/重试、迟到创建及操作取消先RED后GREEN。
3. Preset默认兼容、能力不符回收、只读工具、构造失败、父子权限及Manifest恢复测试。
4. 可选后端真实HTTP请求与Docker服务验证：文件/命令一致、Argv特殊字符、非零退出、CAS冲突、越界链接、输出上限、取消进程和回收。
5. 使用/架构/API文档、完整编译示例和复跑脚本。测试用服务只操作本轮自建容器，密钥不归档。
6. 完整race/vet/lint、独立最终审阅一次；重要发现一次TDD修复，记录未覆盖的边界。

本轮仅本地提交新分支，不自动发布、合并或推送。供应商API资料以固定上游源码为准，网页访问失败时已使用官方Git仓库固定版本核对。
