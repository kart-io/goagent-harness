// Command eval plans or runs the local synthetic planning comparison.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/kart-io/goagent-harness/examples/planning-agent/internal/eval"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if e := run(ctx, os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("eval", flag.ContinueOnError)
	dataset := f.String("dataset", "examples/planning-agent/testdata/eval-cases.json", "本地合成题集")
	out := f.String("out", "", "新输出目录")
	backend := f.String("backend", "plan", "plan、scripted 或 live")
	repeats := f.Int("repeats", 3, "每题独立重复次数")
	model := f.String("model", os.Getenv("OPENAI_MODEL"), "live 模型名称")
	baseURL := f.String("base-url", "", "OpenAI 兼容服务地址，默认读取 OPENAI_BASE_URL")
	temperature := f.Float64("temperature", 0, "live 采样温度")
	if e := f.Parse(args); e != nil {
		return e
	}

	if *out == "" {
		return fmt.Errorf("需要 -out 指定新目录")
	}
	d, e := eval.Load(*dataset)
	if e != nil {
		return e
	}
	m, e := eval.Plan(d, *repeats, *backend)
	if e != nil {
		return e
	}
	var engine eval.Engine
	if *backend == "live" {
		if *model == "" {
			return fmt.Errorf("live 需要 -model 或 OPENAI_MODEL")
		}
		client, err := eval.NewLiveOpenAIClient(*model, *temperature, *baseURL)
		if err != nil {
			return err
		}
		engine = eval.Live{Root: filepath.Join(*out, "state"), Client: client, Model: *model, Temperature: *temperature}
		m.Model, m.Temperature, m.PromptVersion = *model, *temperature, eval.LivePromptVersion
	}
	if *backend == "scripted" {
		engine = eval.Scripted{}
	}
	err := eval.Write(ctx, *out, d, m, engine)
	return errors.Join(err, ctx.Err())
}
