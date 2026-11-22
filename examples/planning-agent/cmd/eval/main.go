// Command eval plans or runs the local synthetic planning comparison.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/kart-io/goagent-harness/examples/planning-agent/internal/eval"
	"os"
)

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(args []string) error {
	f := flag.NewFlagSet("eval", flag.ContinueOnError)
	dataset := f.String("dataset", "examples/planning-agent/testdata/eval-cases.json", "本地合成题集")
	out := f.String("out", "", "新输出目录")
	backend := f.String("backend", "plan", "plan、scripted 或 live")
	repeats := f.Int("repeats", 3, "每题独立重复次数")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *backend == "live" {
		return fmt.Errorf("live 需要配置宿主 Engine 与模型")
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
	if *backend == "scripted" {
		engine = eval.Scripted{}
	}
	return eval.Write(context.Background(), *out, d, m, engine)
}
