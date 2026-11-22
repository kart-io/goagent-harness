package planning

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func replanFixture(t *testing.T) (State, Config, TaskSpec) {
	s, c, a := allocationFixture(t)
	s.NextTaskID = 2
	return s, c, a
}
func acceptedReplanWave(t *testing.T, s State, c Config) State {
	t.Helper()
	n, e := Allocate(s, c, "root")
	require.NoError(t, e)
	require.NotNil(t, n.Wave)
	return acceptWave(t, n, c)
}
func TestReplanReusesAcceptedArtifact(t *testing.T) {
	s, c, a := replanFixture(t)
	s = acceptedReplanWave(t, s, c)
	a.Priority++
	a.Generation = 99
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Equal(t, s.Accepted, n.Accepted)
	require.Equal(t, 1, n.Plan.Tasks[0].Generation)
	require.Equal(t, 2, n.Plan.Revision)
	n.Accepted[0].Content[0] = '9'
	require.Equal(t, "1", string(s.Accepted[0].Content))
}
func TestSupersededSourceInvalidatesDependentClaims(t *testing.T) {
	s, c, a := replanFixture(t)
	b := a.clone()
	b.ID = "t-000002"
	b.DependsOn = []string{a.ID}
	b.Inputs = []InputRef{{a.ID, "out"}}
	z := b.clone()
	z.ID = "t-000003"
	z.DependsOn = []string{b.ID}
	z.Inputs = []InputRef{{b.ID, "out"}}
	s.Plan.Tasks = []TaskSpec{z, b, a}
	s.NextTaskID = 4
	for range 3 {
		s = acceptedReplanWave(t, s, c)
	}
	a.Instruction += " edit"
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{z, b, a}}, c)
	require.NoError(t, e)
	require.Empty(t, n.Accepted)
	for _, task := range n.Plan.Tasks {
		require.Equal(t, 2, task.Generation)
	}
	for _, at := range n.Attempts {
		require.Equal(t, "superseded", at.Status)
		require.NotNil(t, at.Result)
	}
	n, e = Allocate(n, c, "root")
	require.NoError(t, e)
	require.Equal(t, 2, n.Attempts[3].Number)
}
func TestReplanChangedMemberDoesNotReuseAccepted(t *testing.T) {
	s, c, a := replanFixture(t)
	other := c.Members[0]
	other.ID = "other"
	c.Members = append(c.Members, other)
	s = acceptedReplanWave(t, s, c)
	a.MemberID = other.ID
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Empty(t, n.Accepted)
	require.Equal(t, 2, n.Plan.Tasks[0].Generation)
}
func TestReplanNewTaskReferencesAreRewritten(t *testing.T) {
	s, c, a := replanFixture(t)
	s.Plan = nil
	s.NextTaskID = 0
	a.ID = "local"
	s, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Equal(t, "t-000001", s.Plan.Tasks[0].ID)
	a = s.Plan.Tasks[0]
	b := a.clone()
	b.ID = "new:proof"
	a.DependsOn = []string{b.ID}
	a.Inputs = []InputRef{{b.ID, "out"}}
	bad := b.clone()
	bad.ID = "t-000002"
	unchanged, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{bad}}, c)
	require.Error(t, e)
	require.Equal(t, s, unchanged)
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a, b}}, c)
	require.NoError(t, e)
	require.Equal(t, 3, n.NextTaskID)
	require.Equal(t, "t-000002", n.Plan.Tasks[1].ID)
	require.Equal(t, []string{"t-000002"}, n.Plan.Tasks[0].DependsOn)
	require.Equal(t, "t-000002", n.Plan.Tasks[0].Inputs[0].DependencyID)
	require.Equal(t, "new:proof", a.DependsOn[0])
}
func TestWriteTaskCannotBeRecreatedWithNewID(t *testing.T) {
	s, c, a := replanFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []ResourceClaim{{"x", "write"}}
	s = acceptedReplanWave(t, s, c)
	a.ID = "new:replacement"
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Equal(t, "blocked", n.Outcome.Status)
	require.Equal(t, s.Plan, n.Plan)
	require.Equal(t, s.NextTaskID, n.NextTaskID)
}
func TestUnknownEffectCannotCreateAttempt(t *testing.T) {
	s, c, a := replanFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []ResourceClaim{{"x", "write"}}
	s, e := Allocate(s, c, "root")
	require.NoError(t, e)
	s.Wave = nil
	s.Attempts[0].Status = "unknown"
	a.ID = "new:another"
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.Error(t, e)
	require.Equal(t, s, n)
}
func TestWriteFreezeProtectsUnreadyAncestorAcrossRestart(t *testing.T) {
	s, c, a := replanFixture(t)
	m := c.Members[0]
	m.ID = "writer"
	m.EffectClass = "write"
	m.Claims = []ResourceClaim{{"x", "write"}}
	c.Members = append(c.Members, m)
	c.Parallelism = 1
	r := a.clone()
	r.ID = "t-000002"
	a.MemberID = m.ID
	a.Priority = 100
	b := a.clone()
	b.ID = "t-000003"
	b.DependsOn = []string{r.ID}
	b.Inputs = []InputRef{{r.ID, "out"}}
	s.Plan.Tasks = []TaskSpec{a, r, b}
	s.NextTaskID = 4
	s = acceptedReplanWave(t, s, c)
	raw, e := json.Marshal(s)
	require.NoError(t, e)
	var restored State
	require.NoError(t, json.Unmarshal(raw, &restored))
	r.Instruction += "changed"
	n, e := AdoptPlan(restored, PlanDraft{Tasks: []TaskSpec{a, r, b}}, c)
	require.NoError(t, e)
	require.Equal(t, "blocked", n.Outcome.Status)
	require.Equal(t, restored.Plan, n.Plan)
}
func TestReplanStateCapacity(t *testing.T) {
	s, c, a := replanFixture(t)
	a.Instruction += string(make([]byte, 4096))
	c.MaxStateBytes = checkpointBytes(t, s) + 256
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Equal(t, "blocked", n.Outcome.Status)
	require.Equal(t, s.Plan, n.Plan)
	c.MaxStateBytes = 1
	n, e = AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.Error(t, e)
	require.Equal(t, s, n)
}

func TestReplanProtectsAllocatedWriteFingerprint(t *testing.T) {
	s, c, a := replanFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []ResourceClaim{{"x", "write"}}
	s, e := Allocate(s, c, "root")
	require.NoError(t, e)
	s.Wave = nil
	s.Attempts[0].Status = "rejected"
	s.Attempts[0].Fingerprint = "different"
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.NotNil(t, n.Outcome)
	require.Equal(t, "blocked", n.Outcome.Status)
}
func TestReplanHistoricalIDBound(t *testing.T) {
	s, c, a := replanFixture(t)
	c.MaxTasks = 1
	a.ID = "new:replacement"
	n, e := AdoptPlan(s, PlanDraft{Tasks: []TaskSpec{a}}, c)
	require.NoError(t, e)
	require.Equal(t, "blocked", n.Outcome.Status)
	require.Equal(t, s.NextTaskID, n.NextTaskID)
}
