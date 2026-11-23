package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	p "github.com/kart-io/goagent-harness/planning"
	agenterrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

func runExample(t *testing.T, name string) p.State {
	t.Helper()
	initial, cfg, deps, err := scenario(name)
	require.NoError(t, err)
	snap, err := runScenario(t.Context(), t.TempDir(), initial, cfg, deps, "", nil)
	require.NoError(t, err)
	require.True(t, snap.Done)
	require.Equal(t, "succeeded", snap.State.Outcome.Status, snap.State.Outcome.Reason)
	return snap.State
}

func acceptedNamed(t *testing.T, s p.State, name string) p.Artifact {
	t.Helper()
	for _, a := range s.Accepted {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no accepted %s artifact", name)
	return p.Artifact{}
}

func TestSourceScenarioCarriesLocators(t *testing.T) {
	s := runExample(t, "sources")
	require.Equal(t, 2, s.Waves)
	for _, name := range []string{"north", "south"} {
		evidence := acceptedNamed(t, s, name)
		require.Len(t, evidence.Sources, 1)
		ref := evidence.Sources[0]
		require.Equal(t, name, ref.SourceID)
		require.Equal(t, "json_pointer", ref.LocatorKind)
		require.Equal(t, "/count", ref.Locator)
		require.NoError(t, p.ValidateEvidence(ref, s.Goal.Inputs, nil))
		for _, source := range s.Goal.Inputs {
			if source.ID == name {
				require.Equal(t, source.Digest, ref.SourceDigest)
			}
		}
	}
	report := acceptedNamed(t, s, "report")
	require.Len(t, report.Sources, 2)
	for _, derived := range report.Sources {
		require.Equal(t, "/count", derived.Locator)
		require.NoError(t, p.ValidateEvidence(derived, nil, s.Accepted))
	}
	require.JSONEq(t, `{"total":12}`, string(report.Content))
}

type observeReview func(context.Context, p.ReviewInput) (p.Review, error)

func (f observeReview) Review(ctx context.Context, in p.ReviewInput) (p.Review, error) {
	return f(ctx, in)
}

type observeExecution func(context.Context, p.Assignment) (p.TaskResult, error)

func (f observeExecution) Execute(ctx context.Context, in p.Assignment) (p.TaskResult, error) {
	return f(ctx, in)
}

func TestSummaryWaitsForAcceptedEvidence(t *testing.T) {
	initial, cfg, deps, err := scenario("sources")
	require.NoError(t, err)
	var slots [2]atomic.Int32
	var reports atomic.Int32
	executor, reviewer := deps.Executor, deps.Reviewer
	deps.Executor = observeExecution(func(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
		if a.Task.Outputs[0].Name == "report" {
			reports.Add(1)
		} else {
			slots[a.Slot].Add(1)
		}
		return executor.Execute(ctx, a)
	})
	reviewUnavailable := errors.New("south evidence review unavailable")
	deps.Reviewer = observeReview(func(ctx context.Context, in p.ReviewInput) (p.Review, error) {
		if !in.Final && in.Assignment.Task.Outputs[0].Name == "south" {
			return p.Review{}, reviewUnavailable
		}
		return reviewer.Review(ctx, in)
	})
	_, err = runScenario(t.Context(), t.TempDir(), initial, cfg, deps, "", nil)
	require.ErrorIs(t, err, reviewUnavailable)
	require.Equal(t, int32(1), slots[0].Load())
	require.Equal(t, int32(1), slots[1].Load())
	require.Zero(t, reports.Load(), "produced evidence cannot unlock a dependent task")

	s := runExample(t, "sources")
	report := acceptedNamed(t, s, "report")
	require.ElementsMatch(t, []string{acceptedNamed(t, s, "north").ID, acceptedNamed(t, s, "south").ID},
		[]string{report.Sources[0].SourceID, report.Sources[1].SourceID})
}

func TestSourceContentChangeInvalidatesReport(t *testing.T) {
	s := runExample(t, "replan")
	require.Equal(t, 2, s.Plan.Revision)
	require.Equal(t, 1, s.Replans)
	report := acceptedNamed(t, s, "report")
	require.JSONEq(t, `{"total":15}`, string(report.Content))
	var originalReport, originalNorth p.Artifact
	var southAttempts int
	for _, attempt := range s.Attempts {
		for _, a := range attempt.Result.Artifacts {
			switch {
			case a.Name == "south":
				southAttempts++
				require.Equal(t, "accepted", attempt.Status)
				require.Equal(t, acceptedNamed(t, s, "south").ID, a.ID)
			case a.Name == "report" && attempt.Generation == 1:
				require.Equal(t, "superseded", attempt.Status)
				originalReport = a
			case a.Name == "north" && attempt.Generation == 1:
				require.Equal(t, "superseded", attempt.Status)
				originalNorth = a
			}
		}
	}
	require.Equal(t, 1, southAttempts)
	require.JSONEq(t, `{"total":12}`, string(originalReport.Content))
	require.NotEqual(t, originalReport.Digest, report.Digest)
	require.NotEqual(t, originalReport.ID, report.ID)
	north := acceptedNamed(t, s, "north")
	require.NotEqual(t, originalNorth.Sources[0].SourceDigest, north.Sources[0].SourceDigest)
	require.NotEqual(t, originalNorth.Sources[0].QuoteDigest, north.Sources[0].QuoteDigest)
	var staleRef p.EvidenceRef
	for _, ref := range originalReport.Sources {
		if ref.SourceID == originalNorth.ID {
			staleRef = ref
		}
	}
	require.NotEmpty(t, staleRef.SourceID)
	require.Error(t, p.ValidateEvidence(staleRef, nil, s.Accepted))
}

func TestWaitReopensCheckpointAndAnswersSameInterrupt(t *testing.T) {
	initial, cfg, deps, err := scenario("wait")
	require.NoError(t, err)
	dir := t.TempDir()
	waiting, err := runScenario(t.Context(), dir, initial, cfg, deps, "", nil)
	require.ErrorIs(t, err, agenterrors.ErrApprovalRequired)
	require.Len(t, waiting.Interrupts, 1)
	key := waiting.Interrupts[0].Key
	require.NotEqual(t, waiting.Key, key)
	require.Contains(t, string(waiting.Interrupts[0].Info), `"const":true`)

	_, err = runScenario(t.Context(), dir, initial, cfg, deps, key, []byte(`"true"`))
	var rejected *workflow.ResumeRejectedError
	require.ErrorAs(t, err, &rejected)
	refreshed, err := runScenario(t.Context(), dir, initial, cfg, deps, "", nil)
	require.ErrorIs(t, err, agenterrors.ErrApprovalRequired)
	require.Len(t, refreshed.Interrupts, 1)
	require.Equal(t, key, refreshed.Interrupts[0].Key)

	done, err := runScenario(t.Context(), dir, initial, cfg, deps, key, []byte(`true`))
	require.NoError(t, err)
	require.True(t, done.Done)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Len(t, done.State.Attempts, 1)
	require.JSONEq(t, `{"confirmed":true}`, string(acceptedNamed(t, done.State, "report").Content))
}
