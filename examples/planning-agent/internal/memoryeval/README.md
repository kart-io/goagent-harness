# Memory layout evaluation

From the harness root, plan a local LongMemEval dataset in a new directory.

```sh
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out plan
go run ./examples/planning-agent/cmd/memory-eval -dataset oracle.json -out smoke -backend scripted
```

Plan mode writes the manifest without opening a provider. Scripted mode exercises the control flow offline. Explicit live mode accepts the host OpenAI-compatible configuration, `-model` and an optional `-judge-model`. The judge makes a separate request with the reference answer. The host API also accepts three streaming clients that issue at most one physical request per submission. Disable internal retries, request splitting, and redirects. The CLI disables HTTP redirects.

Every session ID must be nonempty and unique within its case; invalid input is rejected before output directories or model calls. Each repeat forms durable memory once per case. Before copying, the host pins up to eight valid records returned by the standard empty-query recall ranking at the fixed case clock. This history-only rule retains each record’s source and never uses questions or reference labels. Set `-core-runes 0` to exclude pinned records from the new layout. The legacy and new layout read independent copies of that store. The off group receives the same original conversation through the recent-history retriever. All groups use the same rune cap, UTF-8 byte-based token estimate, output cap and temperature zero. The CLI defaults to three repeats.

The manifest lists the complete denominator before the first call. Each executed run writes one result per planned key and a summary including shared formation costs. Foreground and judge costs accompany each result. The retained JSON field `physical_requests` counts client submissions, including local failures before transport, rather than independently observed network requests. Cancellation records already buffered provider usage without waiting for the producer to close, and still returns a cancellation error. Monetary charges remain unknown without provider billing. The success rate divides correct answers by all planned trials. The final manifest also records the number of pinned records, compared questions and changed contexts for each case and repeat. A complete comparison with identical contexts reports `no_layout_effect`. Scripted mode exercises request accounting. Dedicated tests check both layouts’ next physical request after forgetting a selected record. The output directory contains source-derived memory stores and generated answers. Use synthetic or authorized data.

[中文说明](README_CN.md)
