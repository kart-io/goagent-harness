// Command memory-eval plans or runs the memory layout ablation on a local LongMemEval dataset.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	"github.com/kart-io/goagent-harness/examples/planning-agent/internal/memoryeval"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
	"github.com/kart-io/goagent/memory/memeval"
	"github.com/kart-io/goagent/testing/mocks"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	c := memoryeval.DefaultConfig()
	f := flag.NewFlagSet("memory-eval", flag.ContinueOnError)
	path := f.String("dataset", "", "LongMemEval 本地 JSON 文件")
	out := f.String("out", "", "新输出目录")
	backend := f.String("backend", "plan", "plan、scripted 或 live")
	model := f.String("model", os.Getenv("OPENAI_MODEL"), "形成与前台模型")
	judge := f.String("judge-model", "", "独立判分模型，默认使用 -model")
	f.IntVar(&c.CoreRunes, "core-runes", c.CoreRunes, "新布局允许钉住记录占用的字符数（0 排除所有钉住记录）")
	f.IntVar(&c.Repeats, "repeats", c.Repeats, "独立重复次数")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *path == "" || *out == "" {
		return fmt.Errorf("需要 -dataset 和 -out")
	}
	input, err := os.Open(*path)
	if err != nil {
		return err
	}
	cases, err := memeval.LoadLongMemEval(input)
	if err = errors.Join(err, input.Close()); err != nil {
		return err
	}
	var cl *memoryeval.Clients
	switch *backend {
	case "plan":
	case "scripted":
		var forms, answers, judges []mocks.ScriptedRule
		for range c.Repeats {
			for _, cs := range cases {
				for range cs.Sessions {
					rule := mocks.ScriptedRule{Name: "形成", Respond: func(_ context.Context, req *llm.CompletionRequest) (mocks.ScriptedReply, error) {
						text := `{"memories":[{"kind":"fact","content":"offline scripted fact","importance":0.8}]}`
						if mocks.RequestContains("你维护长期记忆")(req) {
							text = `{"op":"add"}`
						}
						return mocks.ScriptedReply{Text: text}, nil
					}}
					forms = append(forms, rule, rule)
				}
				for range 3 {
					for range cs.Questions {
						answers = append(answers, mocks.TextReply("作答", "offline scripted answer"))
						judges = append(judges, mocks.TextReply("判分", "no"))
					}
				}
			}
		}
		cl = &memoryeval.Clients{Formation: mocks.NewScriptedClient(forms...), Foreground: mocks.NewScriptedClient(answers...), Judge: mocks.NewScriptedClient(judges...)}
	case "live":
		if *model == "" {
			return fmt.Errorf("live 需要 -model 或 OPENAI_MODEL")
		}
		if *judge == "" {
			*judge = *model
		}
		client, err := service(*model)
		if err != nil {
			return err
		}
		grader, err := service(*judge)
		if err != nil {
			return err
		}
		c.FormationModel, c.ForegroundModel, c.JudgeModel = *model, *model, *judge
		cl = &memoryeval.Clients{Formation: client, Foreground: client, Judge: grader}
	default:
		return fmt.Errorf("未知后端 %q", *backend)
	}
	_, err = memoryeval.Run(ctx, *out, cases, c, cl)
	return err
}

func service(model string) (llm.Client, error) {
	return providers.NewOpenAIWithOptions(llm.WithModel(model), llm.WithHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
}
