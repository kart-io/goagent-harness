package planning

// ProgressItem projects one current-plan task for display without changing its state.
type ProgressItem struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	MemberID string `json:"member_id"`
	Reason   string `json:"reason"`
}

// Progress returns tasks in plan order and permits multiple running tasks.
// It is a display projection, not an execution or recovery authority.
func Progress(s State) []ProgressItem {
	if s.Plan == nil {
		return nil
	}
	latest := make(map[string]Attempt, len(s.Plan.Tasks))
	for _, a := range s.Attempts {
		old, ok := latest[a.TaskID]
		if !ok || a.Generation > old.Generation || a.Generation == old.Generation && a.Number > old.Number {
			latest[a.TaskID] = a
		}
	}
	items := make([]ProgressItem, 0, len(s.Plan.Tasks))
	for _, task := range s.Plan.Tasks {
		item := ProgressItem{TaskID: task.ID, MemberID: task.MemberID, Status: "pending"}
		if a, ok := latest[task.ID]; ok && a.Generation == task.Generation && a.Status != "superseded" {
			item.Status = a.Status
			if a.Result != nil && a.Result.Reason != "" {
				item.Reason = a.Result.Reason
			} else if a.Review != nil {
				item.Reason = a.Review.Reason
			}
		}
		if item.Status == "pending" && s.Outcome != nil && s.Outcome.Status == "blocked" {
			item.Reason = s.Outcome.Reason
		}
		items = append(items, item)
	}
	return items
}
