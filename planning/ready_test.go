package planning

import (
	json "github.com/kart-io/goagent/codec/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestConflictingWritesDoNotShareWave(t *testing.T) {
	s, c, a := allocationFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []ResourceClaim{{"file", "write"}}
	b := a.clone()
	b.ID = "t-000002"
	b.Priority = 10
	s.Plan.Tasks = append(s.Plan.Tasks, b)
	n, e := Allocate(s, c, "root")
	require.NoError(t, e)
	require.Len(t, n.Wave.Assignments, 1)
	require.Equal(t, b.ID, n.Wave.Assignments[0].Task.ID)
	require.NotNil(t, n.WriteFreeze)
	require.Equal(t, 1, n.Waves)
}
func TestAssignmentPreservesGoalConstraintsWithoutExtraSources(t *testing.T) {
	s, c, _ := allocationFixture(t)
	n, e := Allocate(s, c, "r")
	require.NoError(t, e)
	a := n.Wave.Assignments[0]
	require.Equal(t, s.Goal.Constraints, a.GoalContext.Constraints)
	require.Len(t, a.GoalInputs, 1)
	require.Equal(t, "a", a.GoalInputs[0].ID)
	a.GoalInputs[0].Content[0] = '9'
	a.GoalContext.Constraints[0].Rule = "changed"
	require.Equal(t, "1", string(n.Goal.Inputs[0].Content))
	require.Equal(t, "仅草稿", s.Goal.Constraints[0].Rule)
}

func acceptWave(t *testing.T, s State, cfg Config) State {
	t.Helper()
	for _, a := range s.Wave.Assignments {
		r, e := ValidateTaskResult(a, TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "produced", Artifacts: []Artifact{{Name: "out", MediaType: "application/json", Content: []byte("1")}}}, cfg)
		require.NoError(t, e)
		require.Equal(t, "produced", r.Outcome)
		for i := range s.Attempts {
			at := &s.Attempts[i]
			if at.ExecutionKey != a.AttemptKey {
				continue
			}
			at.Status = "accepted"
			at.Result = &r
			d, e := TaskReviewDigest(*at)
			require.NoError(t, e)
			at.Review = &Review{Verdict: "accept", SubjectDigest: d, Reason: "符合要求", Criteria: []CriterionResult{{CriterionID: "local", Status: "pass"}}}
		}
		s.Accepted = append(s.Accepted, cloneSlice(r.Artifacts, Artifact.clone)...)
	}
	s.Wave = nil
	return s
}
func TestReadyRequiresAcceptedDependencyAndIntactArtifact(t *testing.T) {
	s, c, a := allocationFixture(t)
	b := a.clone()
	b.ID = "t-000002"
	b.DependsOn = []string{a.ID}
	b.Inputs = []InputRef{{a.ID, "out"}}
	b.Priority = 100
	s.Plan.Tasks = append(s.Plan.Tasks, b)
	w, err := Allocate(s, c, "r")
	require.NoError(t, err)
	require.Len(t, w.Wave.Assignments, 1)
	require.Equal(t, a.ID, w.Wave.Assignments[0].Task.ID)
	accepted := acceptWave(t, w, c)
	next, err := Allocate(accepted, c, "r")
	require.NoError(t, err)
	require.Len(t, next.Wave.Assignments, 1)
	require.Equal(t, b.ID, next.Wave.Assignments[0].Task.ID)
	require.Equal(t, accepted.Accepted[0], next.Wave.Assignments[0].Inputs[0].Artifact)
	for _, corrupt := range []func(*State){func(s *State) { s.Accepted[0].Content = []byte("2") }, func(s *State) { s.Attempts[0].Result.Artifacts[0].Content = []byte("2") }, func(s *State) { s.Attempts[0].Review.Verdict = "block" }, func(s *State) { s.Attempts[0].Review.SubjectDigest = "stale" }} {
		bad := accepted.Clone()
		corrupt(&bad)
		before := bad.Clone()
		out, e := Allocate(bad, c, "r")
		require.Error(t, e)
		require.Equal(t, before, out)
		require.Equal(t, before, bad)
	}
	done := acceptWave(t, next, c)
	done, err = Allocate(done, c, "r")
	require.NoError(t, err)
	require.Equal(t, "final_review", done.PendingAction)
}
func TestReadyLimitsAndAttemptsAcrossGenerations(t *testing.T) {
	s, c, _ := allocationFixture(t)
	s, err := Allocate(s, c, "r")
	require.NoError(t, err)
	s.Wave = nil
	s.Attempts[0].Status = "rejected"
	s.Plan.Tasks[0].Generation++
	for _, cap := range []string{"wave", "task"} {
		limited := c
		if cap == "wave" {
			limited.MaxWaves = 1
		} else {
			limited.MaxTaskAttempts = 1
		}
		v, e := Allocate(s, limited, "r")
		require.NoError(t, e)
		require.Equal(t, "blocked", v.Outcome.Status)
		require.Len(t, v.Attempts, 1)
	}
	v, e := Allocate(s, c, "r")
	require.NoError(t, e)
	require.Equal(t, 2, v.Attempts[1].Number)
	require.Equal(t, 2, v.Attempts[1].Generation)
	s.Attempts[0].Status = "running"
	v, e = Allocate(s, c, "r")
	require.Error(t, e)
	require.Nil(t, v.Wave)
	require.Len(t, v.Attempts, 1)
}
func TestFreezeRecordsKnownInputsAndBlocksContractChange(t *testing.T) {
	s, c, a := allocationFixture(t)
	writer := c.Members[0]
	writer.ID = "writer"
	writer.EffectClass = "write"
	writer.Claims = []ResourceClaim{{"file", "write"}}
	c.Members = append(c.Members, writer)
	b := a.clone()
	b.ID = "t-000002"
	b.MemberID = writer.ID
	b.DependsOn = []string{a.ID}
	b.Inputs = []InputRef{{a.ID, "out"}}
	s.Plan.Tasks = append(s.Plan.Tasks, b)
	first, e := Allocate(s, c, "r")
	require.NoError(t, e)
	require.Nil(t, first.WriteFreeze)
	upstream := acceptWave(t, first, c)
	v, e := Allocate(upstream, c, "r")
	require.NoError(t, e)
	require.Equal(t, []FrozenOutput{{a.ID, "out", upstream.Accepted[0].Digest}}, v.WriteFreeze.KnownOutputs)
	upstream.WriteFreeze = v.WriteFreeze
	upstream.Plan.Tasks[1].Instruction += "改写"
	blocked, e := Allocate(upstream, c, "r")
	require.NoError(t, e)
	require.Nil(t, blocked.Wave)
	require.Equal(t, "blocked", blocked.Outcome.Status)
	require.Len(t, blocked.Attempts, 1)
}
func TestWaveSizeBoundIsAtomic(t *testing.T) {
	s, c, _ := allocationFixture(t)
	c.MaxStateBytes = checkpointBytes(t, s) + 150
	v, e := Allocate(s, c, "root")
	require.NoError(t, e)
	require.Equal(t, "blocked", v.Outcome.Status)
	require.Empty(t, v.Attempts)
	require.Nil(t, v.Wave)
	require.Zero(t, v.Waves)
}

func TestMissingWaveWithPendingWriteRequiresRecovery(t *testing.T) {
	for _, n := range []int{1, 2} {
		s, c, a := allocationFixture(t)
		c.Members[0].EffectClass = "write"
		c.Members[0].Claims = []ResourceClaim{{"file", "write"}}
		if n == 2 {
			a.ID = "t-000002"
			s.Plan.Tasks = append(s.Plan.Tasks, a)
		}
		w, e := Allocate(s, c, "r")
		require.NoError(t, e)
		replay, e := Allocate(w, c, "r")
		require.NoError(t, e)
		require.Equal(t, w, replay)
		w.Wave = nil
		before := w.Clone()
		got, e := Allocate(w, c, "r")
		require.Error(t, e)
		require.Equal(t, before, got)
		require.Equal(t, before, w)
	}
}
func TestAcceptedRejectsOrphanArtifact(t *testing.T) {
	s, c, _ := allocationFixture(t)
	w, e := Allocate(s, c, "r")
	require.NoError(t, e)
	done := acceptWave(t, w, c)
	orphan := done.Accepted[0].clone()
	orphan.ID += "orphan"
	done.Accepted = append(done.Accepted, orphan)
	before := done.Clone()
	got, e := Allocate(done, c, "r")
	require.Error(t, e)
	require.Equal(t, before, got)
	require.Equal(t, before, done)
}
func TestAcceptedRechecksFrozenOutputLimit(t *testing.T) {
	s, c, _ := allocationFixture(t)
	s.Plan.Tasks[0].Outputs[0].MaxBytes = 1
	w, e := Allocate(s, c, "r")
	require.NoError(t, e)
	done := acceptWave(t, w, c)
	at := &done.Attempts[0]
	v := &at.Result.Artifacts[0]
	v.Content = []byte("11")
	v.Digest, e = artifactDigest(*v)
	require.NoError(t, e)
	done.Accepted[0] = v.clone()
	at.Review.SubjectDigest, e = TaskReviewDigest(*at)
	require.NoError(t, e)
	before := done.Clone()
	got, e := Allocate(done, c, "r")
	require.Error(t, e)
	require.Equal(t, before, got)
	require.Equal(t, before, done)
}
func TestFinalReviewEnforcesWriteFreeze(t *testing.T) {
	s, c, a := allocationFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []ResourceClaim{{"file", "write"}}
	w, e := Allocate(s, c, "r")
	require.NoError(t, e)
	done := acceptWave(t, w, c)
	for _, mode := range []string{"valid", "contract", "output"} {
		t.Run(mode, func(t *testing.T) {
			state := done.Clone()
			switch mode {
			case "contract":
				state.WriteFreeze.ContractDigest = "changed"
			case "output":
				state.WriteFreeze.KnownOutputs = []FrozenOutput{{a.ID, "out", "changed"}}
			}
			before := state.Clone()
			got, e := Allocate(state, c, "r")
			require.NoError(t, e)
			require.Equal(t, before, state)
			if mode == "valid" {
				require.Equal(t, "final_review", got.PendingAction)
				require.Equal(t, []FrozenOutput{{a.ID, "out", state.Accepted[0].Digest}}, got.WriteFreeze.KnownOutputs)
			} else {
				require.NotNil(t, got.Outcome)
				require.Equal(t, "blocked", got.Outcome.Status)
				require.Equal(t, "stop", got.PendingAction)
			}
		})
	}
}

func checkpointBytes(t *testing.T, s State) int {
	t.Helper()
	b, e := json.Marshal(s)
	require.NoError(t, e)
	return len(b)
}
func TestAllocationCapacityLimitsEveryNewCheckpoint(t *testing.T) {
	for _, path := range []string{"final", "waves", "no_ready"} {
		t.Run(path, func(t *testing.T) {
			s, c, a := allocationFixture(t)
			c.Members[0].EffectClass = "write"
			c.Members[0].Claims = []ResourceClaim{{"file", "write"}}
			if path != "final" {
				a.ID = "t-000002"
				s.Plan.Tasks = append(s.Plan.Tasks, a)
			}
			wave, e := Allocate(s, c, "r")
			require.NoError(t, e)
			prior := acceptWave(t, wave, c)
			switch path {
			case "waves":
				c.MaxWaves = 1
			case "no_ready":
				a := prior.Plan.Tasks[1]
				key, e := AttemptKey("r", a.ID, 1, 1)
				require.NoError(t, e)
				prior.Attempts = append(prior.Attempts, Attempt{TaskID: a.ID, EffectClass: "write", ExecutionKey: key, Generation: 1, Number: 1, Status: "rejected"})
			}
			before := prior.Clone()
			c.MaxStateBytes = checkpointBytes(t, prior)
			got, e := Allocate(prior, c, "r")
			require.ErrorContains(t, e, "状态大小上限")
			require.Equal(t, before, got)
			require.Equal(t, before, prior)
			c.MaxStateBytes += 90
			got, e = Allocate(prior, c, "r")
			require.NoError(t, e)
			require.NotNil(t, got.Outcome)
			require.Equal(t, "blocked", got.Outcome.Status)
			require.LessOrEqual(t, checkpointBytes(t, got), c.MaxStateBytes)
			require.Equal(t, before, prior)
		})
	}
}
