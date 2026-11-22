# 记忆布局评估

```sh
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out plan
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out smoke -backend scripted
```

在 harness 根目录运行以上命令。`-dataset` 读取本地 LongMemEval JSON，`-out` 指定新目录。默认离线生成清单，`scripted` 验证控制流，`live` 才连接宿主配置的服务。`-model` 指定形成和前台模型，`-judge-model` 可单独指定判分模型。只有独立判分请求收到参考答案。注入宿主客户端时，每次提交最多发送一个物理请求，须关闭客户端内部重试、请求拆分和重定向。CLI 已关闭 HTTP 重定向。

每个用例每轮只抽取一次，旧布局和新布局各读取一份独立的持久存储副本。关闭组从同一原始历史截取最近会话。三组使用相同字符上限、按 UTF-8 字节估算的 token 上限和输出上限，温度均为零。默认重复三轮。

清单在第一次模型调用前列出所有试验。结果逐题记录终态、答案、前台与判分费用，汇总中单列共享形成费用。未提供账单时金额未知。成功率分母包含灌入失败、评分失败和未开始项。离线结果用于检验控制与删除契约。输出目录包含会话派生的记忆及模型答案，请使用合成或已授权的数据。

[English](README.md)
