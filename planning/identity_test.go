package planning

import (
	"github.com/kart-io/goagent/core/durable"
	"github.com/stretchr/testify/require"
	"testing"
)

func allocationFixture(t *testing.T) (State, Config, TaskSpec) {
	t.Helper()
	g, c, a := validationFixture()
	c.Version = "1"
	c.Members[0].Version = "1"
	c.Members[0].ManifestDigest = "manifest"
	c.Parallelism = 3
	a.Generation = 1
	a.ID = "t-000001"
	g.Constraints = []Constraint{{"draft", "仅草稿"}}
	g.Assumptions = []Assumption{{"assume", "已确认", true}}
	g.Parameters = []GoalParameter{{"date", "today", "host"}}
	for _, id := range []string{"a", "b"} {
		d, e := SourceDigest([]byte("1"))
		require.NoError(t, e)
		g.Inputs = append(g.Inputs, SourceInput{id, id, "application/json", "host", d, []byte("1")})
	}
	a.GoalInputIDs = []string{"a"}
	d, e := GoalDigest(g)
	require.NoError(t, e)
	return State{Goal: g, GoalDigest: d, Plan: &Plan{Revision: 1, GoalDigest: d, Tasks: []TaskSpec{a}}}, c, a
}
func TestAttemptKeyStableAcrossPlanRevision(t *testing.T) {
	s, c, a := allocationFixture(t)
	x, e := Allocate(s, c, "root")
	require.NoError(t, e)
	s.Plan.Revision++
	s.Plan.Tasks[0].Priority++
	y, e := Allocate(s, c, "root")
	require.NoError(t, e)
	require.Equal(t, durable.ChildKey("root", "planning-task/"+a.ID+"/g/1/a/1"), y.Attempts[0].ExecutionKey)
	require.Equal(t, x.Attempts, y.Attempts)
	for _, v := range []struct {
		root, task string
		g, n       int
	}{{"", a.ID, 1, 1}, {"root", " ", 1, 1}, {"root", a.ID, 0, 1}, {"root", a.ID, 1, -1}} {
		_, e := AttemptKey(v.root, v.task, v.g, v.n)
		require.Error(t, e)
	}
}
func TestChangingSelectedGoalInputsInvalidatesAttempt(t *testing.T) {
	s, c, a := allocationFixture(t)
	m := c.Members[0]
	old, e := Fingerprint(s.Goal, a, m, nil)
	require.NoError(t, e)
	a.GoalInputIDs = []string{"b"}
	v, e := Fingerprint(s.Goal, a, m, nil)
	require.NoError(t, e)
	require.NotEqual(t, old, v)
	s.Goal.Inputs[1].Content = []byte("1.0")
	_, e = Fingerprint(s.Goal, a, m, nil)
	require.Error(t, e)
}
func TestChangingMemberWithSameTemplateInvalidatesAcceptedResult(t *testing.T) {
	s, c, a := allocationFixture(t)
	m := c.Members[0]
	old, e := Fingerprint(s.Goal, a, m, nil)
	require.NoError(t, e)
	m.ID = "other"
	_, e = Fingerprint(s.Goal, a, m, nil)
	require.Error(t, e)
	a.MemberID = m.ID
	v, e := Fingerprint(s.Goal, a, m, nil)
	require.NoError(t, e)
	require.NotEqual(t, old, v)
	m.EffectClass = "write"
	m.Claims = []ResourceClaim{{"file", "write"}}
	w, e := Fingerprint(s.Goal, a, m, nil)
	require.NoError(t, e)
	require.NotEqual(t, v, w)
}
func TestGoalDigestIncludesAllGoalFields(t *testing.T) {
	s, _, _ := allocationFixture(t)
	edits := []func(*GoalSpec){func(g *GoalSpec) { g.ID += "2" }, func(g *GoalSpec) { g.Revision++ }, func(g *GoalSpec) { g.Objective += "2" }, func(g *GoalSpec) { g.Constraints[0].Rule += "2" }, func(g *GoalSpec) { g.Deliverables[0].Description += "2" }, func(g *GoalSpec) { g.Criteria[0].Description += "2" }, func(g *GoalSpec) { g.Scope.Excluded = []string{"new"} }, func(g *GoalSpec) { g.Inputs[0].Title += "2" }, func(g *GoalSpec) { g.Parameters[0].Value += "2" }, func(g *GoalSpec) { g.Assumptions[0].Statement += "2" }, func(g *GoalSpec) { g.Output.Language = "en" }}
	for _, edit := range edits {
		g := s.Goal.clone()
		edit(&g)
		d, e := GoalDigest(g)
		require.NoError(t, e)
		require.NotEqual(t, s.GoalDigest, d)
	}
}
