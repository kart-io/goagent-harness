# Memory layout evaluation

From the harness root, plan a local LongMemEval dataset in a new directory.

```sh
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out plan
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out smoke -backend scripted
```

Plan mode writes the manifest without opening a provider. Scripted mode exercises the control flow offline. Explicit live mode accepts the host OpenAI-compatible configuration, `-model` and an optional `-judge-model`. The judge makes a separate request with the reference answer. The host API also accepts three streaming clients that issue at most one physical request per submission. Disable internal retries, request splitting, and redirects. The CLI disables HTTP redirects.

Each repeat forms durable memory once per case. The legacy and new layout read independent copies of that store. The off group receives the same original conversation through the recent-history retriever. All groups use the same rune cap, UTF-8 byte-based token estimate, output cap and temperature zero. The CLI defaults to three repeats.

The manifest lists the complete denominator before the first call. Each executed run writes one result per planned key and a summary including shared formation costs. Foreground and judge costs accompany each result. Monetary charges remain unknown without provider billing. The success rate divides correct answers by all planned trials. Scripted results exercise accounting and deletion contracts. The output directory contains source-derived memory stores and generated answers. Use synthetic or authorized data.

[中文说明](README_CN.md)
