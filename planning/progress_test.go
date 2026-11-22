package planning_test

import (
	"testing"

	"github.com/kart-io/goagent-harness/planning"
	"github.com/stretchr/testify/require"
)

func TestProgressProjectsParallelAndCurrentGeneration(t *testing.T) {
	s := planning.State{Plan: &planning.Plan{Tasks: []planning.TaskSpec{
		{ID: "a", MemberID: "reader", Generation: 1},
		{ID: "b", MemberID: "reader", Generation: 1},
		{ID: "c", MemberID: "writer", Generation: 2},
	}}, Attempts: []planning.Attempt{
		{TaskID: "a", Generation: 1, Number: 1, Status: "running"},
		{TaskID: "b", Generation: 1, Number: 1, Status: "running"},
		{TaskID: "c", Generation: 1, Number: 1, Status: "accepted"},
	}}
	before := s.Clone()
	got := planning.Progress(s)
	require.Equal(t, []planning.ProgressItem{
		{TaskID: "a", MemberID: "reader", Status: "running"},
		{TaskID: "b", MemberID: "reader", Status: "running"},
		{TaskID: "c", MemberID: "writer", Status: "pending"},
	}, got)
	require.Equal(t, before, s, "展示投影不得修改权威状态")
	got[0].Status = "accepted"
	require.Equal(t, "running", s.Attempts[0].Status)
	got = planning.Progress(planning.State{})
	require.Empty(t, got)
}

func TestProgressReportsLatestAttemptReason(t *testing.T) {
	s := planning.State{Plan: &planning.Plan{Tasks: []planning.TaskSpec{{ID: "t", MemberID: "r", Generation: 1}}},
		Attempts: []planning.Attempt{
			{TaskID: "t", Generation: 1, Number: 2, Status: "rejected", Result: &planning.TaskResult{Reason: "来源不足"}},
			{TaskID: "t", Generation: 1, Number: 1, Status: "produced"},
		}}
	require.Equal(t, []planning.ProgressItem{{TaskID: "t", MemberID: "r", Status: "rejected", Reason: "来源不足"}}, planning.Progress(s))
}
