package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"strings"

	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/llm"
)

// ComponentLoopDetect 是循环检测组件名。
const ComponentLoopDetect = "loop-detect"

// LoopDetectConfig 是 loop-detect 组件的配置。每步请求模型前检查最近的工具调用：同一工具以相同参数调用且结果相同
// 达到 Repeats 次，同一文件被编辑达到 FileEdits 次，或相近的 run_command 命令达到 CommandRepeats 次时，
// 在工具结果之后追加一条提醒，要求模型重新考虑做法。
// 判断只依据消息历史，提醒带标记，持久运行恢复时结果一致。只作用于原生工具流。
type LoopDetectConfig struct {
	// Window 是参与统计的最近工具调用数，默认 30。
	Window int `yaml:"window"`
	// Repeats 是相同调用与结果的提醒阈值，默认 3，至少 2。
	Repeats int `yaml:"repeats"`
	// FileEdits 是同一文件的编辑提醒阈值，默认 8。
	FileEdits int `yaml:"fileEdits"`
	// CommandRepeats 是相近命令的提醒阈值，默认 6。命令只比较第一行、管道之前的部分，并去掉重定向与
	// -count=N 这类带值的参数：反复运行同一测试、只换输出过滤方式，或用 heredoc 反复重写同一个文件，
	// 每次结果都不同，按相同调用统计发现不了。
	CommandRepeats int `yaml:"commandRepeats"`
	// EditTools 是计入文件编辑的工具，按 path 参数统计，默认 write_file、edit_file、multi_edit。
	EditTools []string `yaml:"editTools"`
	// HaltRepeats 大于 0 时，相同调用与结果达到该次数即结束本轮并说明原因；须大于 Repeats。默认 0 不结束。
	HaltRepeats int `yaml:"haltRepeats"`
}

var loopDetectSpec = Spec[LoopDetectConfig]{
	Name: ComponentLoopDetect, Version: "2", Singleton: true,
	Validate: func(c *LoopDetectConfig) error {
		if c.Window < 0 || c.Repeats < 0 || c.FileEdits < 0 || c.CommandRepeats < 0 || c.HaltRepeats < 0 {
			return errors.New("window、repeats、fileEdits、commandRepeats、haltRepeats 不能为负")
		}
		if c.Window == 0 {
			c.Window = 30
		}
		if c.Repeats == 0 {
			c.Repeats = 3
		}
		if c.FileEdits == 0 {
			c.FileEdits = 8
		}
		if c.CommandRepeats == 0 {
			c.CommandRepeats = 6
		}
		if c.EditTools == nil {
			c.EditTools = []string{coding.ToolWriteFile, coding.ToolEditFile, coding.ToolMultiEdit}
		}
		if c.Repeats < 2 {
			return errors.New("repeats 至少为 2")
		}
		if c.HaltRepeats > 0 && c.HaltRepeats <= c.Repeats {
			return errors.New("haltRepeats 须大于 repeats")
		}
		return nil
	},
	Build: func(_ context.Context, a *Assembler, c *LoopDetectConfig) error {
		cfg := *c
		a.AddPreStep(func(_ context.Context, step *builder.Step) (builder.PreStepResult, error) {
			d := detectLoop(step.Messages, cfg)
			if d.halt != "" {
				return builder.PreStepResult{Halt: true, Answer: d.halt}, nil
			}
			if d.warning != "" {
				step.Messages = append(step.Messages, llm.UserMessage(d.warning))
			}
			return builder.PreStepResult{}, nil
		})
		return nil
	},
}

func init() { Register(loopDetectSpec) }

type loopDecision struct{ warning, halt string }

// loopSignal 是一类重复：相同的调用与结果、对同一文件的编辑，或相近的命令。
type loopSignal struct {
	marker    string
	count     int
	last      int // 最近一次出现所在的消息下标
	text      func(count int) string
	threshold int
	repeat    bool // 相同调用与结果，HaltRepeats 只对它生效
}

const loopMarkerPrefix = "[loop-detect:"

// detectLoop 只在最后一条消息是工具结果时判断：用户刚给出新输入时不打断。
// 每个信号在最近一次出现之后提醒过就不再重复提醒。
func detectLoop(msgs []llm.Message, c LoopDetectConfig) loopDecision {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "tool" {
		return loopDecision{}
	}
	results := map[string]string{}
	warned := map[string]int{}
	type call struct {
		index          int
		id, name, args string
	}
	var calls []call
	for i, m := range msgs {
		switch m.Role {
		case "tool":
			results[m.ToolCallID] = m.Content
		case "user":
			for rest := m.Content; ; {
				start := strings.Index(rest, loopMarkerPrefix)
				if start < 0 {
					break
				}
				rest = rest[start+len(loopMarkerPrefix):]
				if end := strings.IndexByte(rest, ']'); end > 0 {
					warned[rest[:end]] = i
				}
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				calls = append(calls, call{index: i, id: tc.ID, name: tc.Function.Name, args: tc.Function.Arguments})
			}
		}
	}
	if len(calls) > c.Window {
		calls = calls[len(calls)-c.Window:]
	}
	signals := map[string]*loopSignal{}
	var order []string
	bump := func(key string, index, threshold int, repeat bool, text func(int) string) {
		s, ok := signals[key]
		if !ok {
			s = &loopSignal{marker: loopMarker(key), text: text, threshold: threshold, repeat: repeat}
			signals[key] = s
			order = append(order, key)
		}
		s.count++
		s.last = index
	}
	for _, cl := range calls {
		args := canonicalJSON(cl.args, "")
		name := cl.name
		bump("call\x00"+name+"\x00"+args+"\x00"+canonicalJSON(results[cl.id], "duration"), cl.index, c.Repeats, true, func(n int) string {
			return fmt.Sprintf("你已用相同参数调用 %s %d 次，结果都相同。重复执行不会带来新进展：先停下来分析原因，换一种做法。", name, n)
		})
		if slices.Contains(c.EditTools, name) {
			var a struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(cl.args), &a) == nil && a.Path != "" {
				path := a.Path
				bump("edit\x00"+path, cl.index, c.FileEdits, false, func(n int) string {
					return fmt.Sprintf("你已修改 %s %d 次。不要继续局部修补，回到任务要求重新审视整体方案。", path, n)
				})
			}
		}
		if name == coding.ToolRunCommand {
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(cl.args), &a) == nil {
				if family := commandFamily(a.Command); family != "" {
					bump("command\x00"+family, cl.index, c.CommandRepeats, false, func(n int) string {
						return fmt.Sprintf("你已运行相近的命令 %d 次：%s。反复调整同一条命令不会带来新信息：根据已有输出分析原因再动手；"+
							"失败若来自环境限制而不是本次改动，在答复中说明即可，不要继续排查。", n, family)
					})
				}
			}
		}
	}
	var lines []string
	for _, key := range order {
		s := signals[key]
		if s.repeat && c.HaltRepeats > 0 && s.count >= c.HaltRepeats {
			return loopDecision{halt: "已停止本轮：" + s.text(s.count) + "请调整任务或给出新的指引后再继续。"}
		}
		if last, ok := warned[s.marker]; s.count >= s.threshold && (!ok || last < s.last) {
			lines = append(lines, loopMarkerPrefix+s.marker+"] "+s.text(s.count))
		}
	}
	return loopDecision{warning: strings.Join(lines, "\n")}
}

var (
	commandRedirect  = regexp.MustCompile(`\s\d?>(&\d|\s*/dev/null)`)
	commandValueFlag = regexp.MustCompile(`\s-{1,2}[\w.-]+=\S+`)
)

// commandFamily 把命令归到一类：只取第一行（heredoc 正文不算）与第一个管道之前的部分，去掉 2>&1、>/dev/null 与
// -flag=value 参数；写入文件的重定向保留，重写不同文件不算同一类。
func commandFamily(command string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	line, _, _ = strings.Cut(line, " | ")
	line = commandValueFlag.ReplaceAllString(" "+line, "")
	line = commandRedirect.ReplaceAllString(line, "")
	return strings.Join(strings.Fields(line), " ")
}

func loopMarker(key string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("%08x", h.Sum32())
}

// canonicalJSON 让参数顺序不同的同一调用得到相同的键，并去掉每次都会变化的字段；不是 JSON 时原样返回。
func canonicalJSON(s, drop string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	if m, ok := v.(map[string]any); ok && drop != "" {
		delete(m, drop)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}
