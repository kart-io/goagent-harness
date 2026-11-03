# SWE-bench Multilingual Go 子集评测

在 [SWE-bench Multilingual](https://www.swebench.com/multilingual.html) 的 Go 子集（42 题：caddy、gin、prometheus、hugo、terraform）上评测 goagent-harness 装配的编码 Agent。Agent 在 native 沙箱中解题，补丁由官方镜像和官方评测脚本判定。

## 准备

- Docker，能拉取 `swebench/sweb.eval.x86_64.*` 镜像（每题一个，数 GB）。
- bubblewrap（native 沙箱）。
- 数据集：从 Hugging Face 的 `SWE-bench/SWE-bench_Multilingual` 导出 Go 题为 JSON 数组，字段同原数据集。
- 模型的 API Key：`-model deepseek` 需要 `DEEPSEEK_API_KEY`，`-model minimax` 需要 `MINIMAX_CN_API_KEY`。

## 运行

```bash
go build -o swebench ./eval/swebench
./swebench -data go.json -out results/minimax -model minimax -parallel 4
```

- 每题先用官方补丁（gold）校验评测环境，未通过的题不跑 Agent。
- 中断后用同样的命令续跑，跳过已完成的运行；因模型服务出错或评测环境出错的运行会重做。
- 每题的目录里有 `agent.log`（工具调用）、`requests.log`（每次模型请求的耗时与用量）、`patch.diff` 和 `eval.log`。
- `-runs 3` 每题跑 3 次，用于估计波动。

## 结果与基线

运行结束时写出 `<out>/summary.json`：解决率、分类计数、平均耗时与模型请求数、每题结果。环境与模型服务失败不计入分母。

| 分类 | 含义 |
|---|---|
| `resolved` | 已解决 |
| `wrong_fix` | 应修复的测试仍未通过 |
| `regression` | 应修复的测试通过，但原本通过的测试失败 |
| `no_patch` | 没有改动 |
| `apply_failed` | 补丁无法应用 |
| `agent_stopped` | Agent 未正常结束：超时、步数用尽、输出被截断等 |
| `model_service` | 模型请求失败（503、断流等）而中止 |
| `env` | 镜像、评测脚本等环境问题 |

改动 harness 后，与之前的结果比较：

```bash
./swebench -data go.json -out results/new -model minimax -baseline results/minimax/summary.json
```

打印退步（基线解决、本次未解决）与进步的题，有退步时退出码为 2，可直接用于定时任务报警。`-mode report` 只汇总已有结果，不运行；`-ids @regression.txt` 只跑文件中列出的题（每行一题，`#` 开头为注释）。
