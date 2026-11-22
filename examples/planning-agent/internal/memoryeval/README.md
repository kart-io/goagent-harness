# Memory layout evaluation

From the harness root, plan a local LongMemEval dataset in a new directory.

```sh
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out plan
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out smoke -backend scripted
```

Plan mode writes the manifest without opening a provider. Scripted mode exercises the control flow offline. Explicit live mode accepts the host OpenAI-compatible configuration, `-model` and an optional `-judge-model`. The judge makes a separate request with the reference answer. The host API also accepts three single-attempt streaming clients.

Each repeat forms durable memory once per case. The legacy and new layout read independent copies of that store. The off group receives the same original conversation through the recent-history retriever. All groups use the same rune cap, UTF-8 byte-based token estimate, output cap and temperature zero. The CLI defaults to three repeats.

The manifest lists the complete denominator before the first call. Each executed run writes one result per planned key and a summary including shared formation costs. Foreground and judge costs accompany each result. Monetary charges remain unknown without provider billing. The success rate divides correct answers by all planned trials. Scripted results exercise accounting and deletion contracts. The output directory contains source-derived memory stores and generated answers. Use synthetic or authorized data.

# 记忆布局评估

在 harness 根目录运行以上命令。`-dataset` 读取本地 LongMemEval JSON，`-out` 指定新目录。默认离线生成清单，`scripted` 验证控制流，`live` 才连接宿主配置的服务。`-model` 指定形成和前台模型，`-judge-model` 可单独指定判分模型。只有独立判分请求收到参考答案。

每个用例每轮只抽取一次，旧布局和新布局各读取一份独立的持久存储副本。关闭组从同一原始历史截取最近会话。三组使用相同字符上限、按 UTF-8 字节估算的 token 上限和输出上限，温度均为零。默认重复三轮。

清单在第一次模型调用前列出所有试验。结果逐题记录终态、答案、前台与判分费用，汇总中单列共享形成费用。未提供账单时金额未知。成功率分母包含灌入失败、评分失败和未开始项。离线结果用于检验控制与删除契约。输出目录包含会话派生的记忆及模型答案，请使用合成或已授权的数据。
