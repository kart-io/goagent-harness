package planning

import "context"

type agentGoalResolver struct{ *planningAgent }

// NewAgentGoalResolver constructs a journaled resolver with the same host
// factory requirements as NewAgentPlanner.
func NewAgentGoalResolver(cfg AgentConfig) (GoalResolver, error) {
	a, err := newPlanningAgent(cfg, "planning_resolve", "将请求解析为目标，缺少材料、参数、范围或截止时间时列出澄清问题。仅选择请求或回答提供的材料，原样保留来源。只返回 GoalResolution JSON 对象，含 candidate 和 questions。不得自行确认假设。")
	if err != nil {
		return nil, err
	}
	return agentGoalResolver{a}, nil
}
func (a agentGoalResolver) Resolve(ctx context.Context, in GoalResolveInput) (GoalResolution, error) {
	var out GoalResolution
	if err := a.candidate(ctx, in, &out); err != nil {
		return GoalResolution{}, err
	}
	trusted := map[string]SourceInput{}
	add := func(inputs []SourceInput) error {
		for _, s := range inputs {
			digest, err := SourceDigest(s.Content)
			if err != nil || s.ID == "" || digest != s.Digest {
				return invalid("来源 %q 无效", s.ID)
			}
			if old, ok := trusted[s.ID]; ok && (old.Digest != s.Digest || old.Title != s.Title || old.MediaType != s.MediaType || old.Origin != s.Origin) {
				return invalid("来源 %q 重复且不一致", s.ID)
			}
			trusted[s.ID] = s
		}
		return nil
	}
	if err := add(in.Request.Inputs); err != nil {
		return GoalResolution{}, err
	}
	for _, answer := range in.Answers {
		if err := add(answer.Inputs); err != nil {
			return GoalResolution{}, err
		}
	}
	seen := map[string]bool{}
	for i, s := range out.Candidate.Inputs {
		actual, ok := trusted[s.ID]
		digest, err := SourceDigest(s.Content)
		if !ok || seen[s.ID] || err != nil || digest != actual.Digest || s.Digest != digest || s.Title != actual.Title || s.MediaType != actual.MediaType || s.Origin != actual.Origin {
			return GoalResolution{}, invalid("候选使用未授权或不一致的来源 %q", s.ID)
		}
		seen[s.ID] = true
		out.Candidate.Inputs[i] = actual.clone()
	}
	return out, nil
}
