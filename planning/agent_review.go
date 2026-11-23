package planning

import "context"

type agentReviewer struct{ *planningAgent }

// NewAgentReviewer constructs a journaled reviewer with the same host factory
// requirements as NewAgentPlanner.
func NewAgentReviewer(cfg AgentConfig) (Reviewer, error) {
	a, err := newPlanningAgent(cfg, "planning_review", "按冻结对象逐项复核验收条件与证据，证据不足不能通过。只返回 Review JSON 对象，含 subject_digest、verdict、reason、criteria、issues、repair。原样复制输入的 subject_digest。")
	if err != nil {
		return nil, err
	}
	return agentReviewer{a}, nil
}
func (a agentReviewer) Review(ctx context.Context, in ReviewInput) (Review, error) {
	var out Review
	if err := a.candidate(ctx, in, &out); err != nil {
		return Review{}, err
	}
	if in.SubjectDigest == "" || in.SubjectDigest != out.SubjectDigest {
		return Review{}, invalid("复核目标摘要与冻结对象不符")
	}
	return out, nil
}
