package planning

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/observe"
)

type planningGraph struct {
	cfg  Config
	deps Dependencies
}

func trustedSources(req []SourceInput, answers []GoalAnswer, c Config) (map[string]SourceInput, error) {
	sources := map[string]SourceInput{}
	remaining := defaultLimit(c.MaxArtifactTotalBytes, 1048576)
	add := func(items []SourceInput) error {
		local := idSet{}
		for _, v := range items {
			if !local.add(v.ID) || !nonempty(v.Origin) || !nonempty(v.Title) || !nonempty(v.MediaType) || len(v.Content) > defaultLimit(c.MaxArtifactBytes, 65536) {
				return invalid("材料身份、元数据或大小无效")
			}
			d, e := SourceDigest(v.Content)
			if e != nil || d != v.Digest {
				return invalid("材料摘要不符")
			}
			if old, ok := sources[v.ID]; ok {
				if old.Digest != d || old.Origin != v.Origin || old.Title != v.Title || old.MediaType != v.MediaType {
					return invalid("材料身份冲突")
				}
				continue
			}
			remaining -= len(v.Content)
			if remaining < 0 {
				return invalid("材料超过总量限制")
			}
			sources[v.ID] = v
		}
		return nil
	}
	if e := add(req); e != nil {
		return nil, e
	}
	for _, a := range answers {
		if e := add(a.Inputs); e != nil {
			return nil, e
		}
	}
	return sources, nil
}

func (f planningGraph) normalize(ctx context.Context, s State) (State, error) {
	s, err := prepareInitial(s, f.cfg)
	if err != nil {
		return s, err
	}
	if s.PendingAction == "stop" && s.Outcome != nil && s.Outcome.Status == "blocked" {
		return s, nil
	}
	if s.Request == nil {
		if e := ValidateGoalPolicy(s.Goal, f.cfg); e != nil {
			return blocked(s, e.Error()), nil
		}
		d, e := GoalDigest(s.Goal)
		s.GoalDigest = d
		s.PendingAction = "plan"
		return s, e
	}
	if f.deps.GoalResolver == nil {
		return s, configError("自然语言目标需要 GoalResolver")
	}
	if !nonempty(s.Request.Text) {
		return blocked(s, "请求文本缺失"), nil
	}
	if _, e := trustedSources(s.Request.Inputs, nil, f.cfg); e != nil {
		return blocked(s, e.Error()), nil
	}
	if i := s.Request.Initial; i != nil {
		s.Goal.ID = i.ID
		s.Goal.Revision = i.Revision
	}
	if s.Goal.ID == "" {
		s.Goal.ID = durable.ChildKey(observe.RunIDFromContext(ctx), "goal")
	}
	if s.Goal.Revision == 0 {
		s.Goal.Revision = 1
	}
	s.PendingAction = "resolve_goal"
	return s, nil
}

func (f planningGraph) resolve(ctx context.Context, s State) (State, error) {
	round := s.GoalRounds + 1
	reject := func(reason string) (State, error) {
		s.GoalResolutionError = boundedText(reason)
		s.PendingAction = "resolve_goal"
		if round >= f.cfg.MaxGoalRounds {
			return blocked(s, s.GoalResolutionError), nil
		}
		return s, nil
	}
	if round > f.cfg.MaxGoalRounds {
		return blocked(s, "已达目标解析轮数上限"), nil
	}
	copy := s.Clone()
	in := GoalResolveInput{Request: *copy.Request, Previous: copy.Resolution, Answers: copy.GoalAnswers, Round: round, RejectionReason: s.GoalResolutionError}
	if e := withinStateLimit(in, f.cfg); e != nil {
		return blocked(s, e.Error()), nil
	}
	res, err := f.deps.GoalResolver.Resolve(ctx, in)
	var bad *CandidateError
	if err != nil && !errors.As(err, &bad) {
		return s, err
	}
	s.GoalRounds = round
	if err != nil {
		return reject(err.Error())
	}
	if e := withinStateLimit(res, f.cfg); e != nil {
		return reject(e.Error())
	}
	res = res.clone()
	g := &res.Candidate
	g.ID = s.Goal.ID
	g.Revision = s.Goal.Revision
	for i := range g.Assumptions {
		g.Assumptions[i].Confirmed = false
	}
	sources, e := trustedSources(s.Request.Inputs, s.GoalAnswers, f.cfg)
	if e != nil {
		return s, e
	}
	seen := idSet{}
	for i, v := range g.Inputs {
		actual, ok := sources[v.ID]
		bodyDigest, err := SourceDigest(v.Content)
		if !ok || !seen.add(v.ID) || err != nil || v.Digest != actual.Digest || bodyDigest != actual.Digest || v.Title != actual.Title || v.MediaType != actual.MediaType || v.Origin != actual.Origin {
			return reject("候选材料与宿主材料不符")
		}
		g.Inputs[i] = actual.clone()
	}
	fields := idSet{}
	names := []string{}
	for i, q := range res.Questions {
		if !fields.add(q.Field) || !nonempty(q.Prompt) {
			return reject("澄清字段或提问无效")
		}
		res.Questions[i].ID = fmt.Sprintf("%d/%s", round, q.Field)
		names = append(names, q.Field)
	}
	s.GoalResolutionError = ""
	s.Resolution = &res
	if len(names) > 0 {
		if round >= f.cfg.MaxGoalRounds {
			return reject("仍需澄清 " + strings.Join(names, "、"))
		}
		s.PendingAction = "clarify_goal"
		return s, nil
	}
	confirmed := g.clone()
	for i := range confirmed.Assumptions {
		confirmed.Assumptions[i].Confirmed = true
	}
	if e := ValidateGoalPolicy(confirmed, f.cfg); e != nil {
		missing := []string{}
		for _, key := range f.cfg.RequiredGoalParameters {
			if !slices.ContainsFunc(confirmed.Parameters, func(p GoalParameter) bool { return p.Key == key && nonempty(p.Value) }) {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return reject("缺少必需目标参数 " + strings.Join(missing, "、"))
		}
		return reject(e.Error())
	}
	s.PendingAction = "confirm_goal"
	return s, nil
}

func parseAnswers(raw []byte, s State, c Config, d string) ([]GoalAnswer, error) {
	var a struct {
		RequestDigest string       `json:"request_digest"`
		Round         int          `json:"round"`
		Answers       []GoalAnswer `json:"answers"`
	}
	if e := DecodeCandidate(raw, &a); e != nil {
		return nil, e
	}
	if len(raw) > defaultLimit(c.MaxStateBytes, 8388608) || a.RequestDigest != d || a.Round != s.GoalRounds {
		return nil, invalid("回答未绑定本次请求或轮次")
	}
	pending := idSet{}
	for _, q := range s.Resolution.Questions {
		pending[q.ID] = true
	}
	for _, v := range a.Answers {
		if !pending[v.QuestionID] || (!nonempty(v.Value) && len(v.Inputs) == 0) {
			return nil, invalid("回答缺失或问题身份无效")
		}
		delete(pending, v.QuestionID)
	}
	if len(pending) > 0 {
		return nil, invalid("回答不完整")
	}
	answers := append(s.Clone().GoalAnswers, a.Answers...)
	if _, e := trustedSources(s.Request.Inputs, answers, c); e != nil {
		return nil, e
	}
	return answers, nil
}

func (f planningGraph) clarify(ctx context.Context, s State) (State, error) {
	d, e := digestJSON(s.Request)
	if e != nil {
		return s, e
	}
	candidate, e := GoalDigest(s.Resolution.Candidate)
	if e != nil {
		return s, e
	}
	info, e := json.Marshal(map[string]any{"request_digest": d, "candidate_digest": candidate,
		"request_summary": boundedText(s.Request.Text), "candidate_summary": boundedText(s.Resolution.Candidate.Objective),
		"round": s.GoalRounds, "questions": s.Resolution.Questions})
	if e != nil {
		return s, e
	}
	raw, e := durable.AwaitInput(ctx, fmt.Sprintf("goal/questions/%d", s.GoalRounds), info, func(raw json.RawMessage) error { _, e := parseAnswers(raw, s, f.cfg, d); return e })
	if e != nil {
		return s, e
	}
	s.GoalAnswers, e = parseAnswers(raw, s, f.cfg, d)
	s.PendingAction = "resolve_goal"
	return s, e
}

func (f planningGraph) confirm(ctx context.Context, s State) (State, error) {
	g := s.Resolution.Candidate.clone()
	d, e := GoalDigest(g)
	if e != nil {
		return s, e
	}
	info, e := json.Marshal(map[string]any{"candidate_digest": d, "candidate": g})
	if e != nil {
		return s, e
	}
	type approval struct {
		Digest   string `json:"candidate_digest"`
		Approved *bool  `json:"approved"`
	}
	raw, e := durable.AwaitInput(ctx, fmt.Sprintf("goal/confirm/%d", s.GoalRounds), info, func(raw json.RawMessage) error {
		var a approval
		if e := DecodeCandidate(raw, &a); e != nil {
			return e
		}
		if a.Digest != d || a.Approved == nil {
			return invalid("确认信息与候选不符")
		}
		return nil
	})
	if e != nil {
		return s, e
	}
	var a approval
	if e = DecodeCandidate(raw, &a); e != nil {
		return s, e
	}
	if !*a.Approved {
		return blocked(s, "目标未获确认"), nil
	}
	for i := range g.Assumptions {
		g.Assumptions[i].Confirmed = true
	}
	if e = ValidateGoalPolicy(g, f.cfg); e != nil {
		return s, e
	}
	s.Goal = g
	s.GoalDigest, e = GoalDigest(g)
	s.PendingAction = "plan"
	return s, e
}
