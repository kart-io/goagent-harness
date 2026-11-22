package planning

import "context"

type agentPlanner struct{ *planningAgent }

// NewAgentPlanner constructs a journaled planner. The host must supply fresh,
// read-only Builder sessions with stable AgentID and durable scope, the parent's
// journal, permissions and budget, and interrupted model resends disabled.
// The adapter cannot inspect the tool registry of an opaque factory.
func NewAgentPlanner(cfg AgentConfig) (Planner, error) {
	a, err := newPlanningAgent(cfg, "planning_plan", "根据封存目标、成员能力、已有产物和反馈规划任务依赖与验收条件。只返回 PlanDraft JSON 对象，含 tasks、groups、rationale。")
	if err != nil {
		return nil, err
	}
	return agentPlanner{a}, nil
}
func (a agentPlanner) Plan(ctx context.Context, in PlanningInput) (PlanDraft, error) {
	var out PlanDraft
	if err := a.candidate(ctx, in, &out); err != nil {
		return PlanDraft{}, err
	}
	return out, nil
}
