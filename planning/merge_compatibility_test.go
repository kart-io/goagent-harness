package planning

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

// A committed first wave and a waiting second wave distinguish checkpoint
// compatibility from simply rerunning a fresh graph or comparing digests.
func mergeCompatibilityFixture(t *testing.T) (State, Config, Dependencies, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	s, c, d := graphFixture(t)
	executor := d.Executor
	first, second := new(atomic.Int32), new(atomic.Int32)
	d.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
		if a.Task.ID == "t-000002" {
			_, err := durable.AwaitInput(ctx, "merge-member-gate", []byte(`{"confirm":true}`), nil)
			if err != nil {
				return TaskResult{}, err
			}
			second.Add(1)
		} else {
			first.Add(1)
		}
		return executor.Execute(ctx, a)
	})
	return s, c, d, first, second
}

func TestMergeMemberManifestChangeRequiresNewRun(t *testing.T) {
	s, c, d, first, second := mergeCompatibilityFixture(t)
	dir := t.TempDir()
	open := func() durable.Store {
		store, err := durable.NewFileStore(dir)
		require.NoError(t, err)
		return store
	}
	ctx := tools.WithSessionID(t.Context(), "before-upgrade")
	waiting, err := compileFixture(t, c, d, open()).Run(ctx, "old-run", s, "")
	require.ErrorIs(t, err, agentErrors.ErrApprovalRequired)
	require.Len(t, waiting.Interrupts, 1)
	require.Len(t, waiting.State.Accepted, 1)
	require.Equal(t, int32(1), first.Load())
	require.Zero(t, second.Load())

	migrated := c
	migrated.Members = append([]Member(nil), c.Members...)
	migrated.Members[0].ManifestDigest += "/skills-v3"
	resume := workflow.ResumeRequest{Key: waiting.Interrupts[0].Key, Input: []byte("true")}
	for _, sessionID := range []string{"before-upgrade", "after-upgrade"} {
		t.Run(sessionID, func(t *testing.T) {
			// Reopen the on-disk journal and recompile with the new directory.
			g := compileFixture(t, migrated, d, open())
			changedCtx := tools.WithSessionID(t.Context(), sessionID)
			_, err := g.Snapshot(changedCtx, "old-run")
			require.True(t, agentErrors.IsCode(err, agentErrors.CodeStateValidation), "%v", err)
			require.ErrorContains(t, err, "checkpoint does not match versioned graph")
			_, err = g.Resume(changedCtx, "old-run", resume)
			require.True(t, agentErrors.IsCode(err, agentErrors.CodeStateValidation), "%v", err)
			require.ErrorContains(t, err, "checkpoint does not match versioned graph")
			require.Equal(t, int32(1), first.Load(), "rejected migration must not execute either wave")
			require.Zero(t, second.Load())
		})
	}

	// The failed resumes must leave the original continuation usable.
	unchanged, err := compileFixture(t, c, d, open()).Snapshot(ctx, "old-run")
	require.NoError(t, err)
	require.Equal(t, waiting.State, unchanged.State)
	require.Equal(t, waiting.Interrupts, unchanged.Interrupts)

	newCtx := tools.WithSessionID(t.Context(), "after-upgrade")
	fresh, err := compileFixture(t, migrated, d, open()).Run(newCtx, "new-run", s, "")
	require.ErrorIs(t, err, agentErrors.ErrApprovalRequired)
	require.Len(t, fresh.Interrupts, 1)
	require.Equal(t, int32(2), first.Load(), "a new run executes its own first wave")
	done, err := compileFixture(t, migrated, d, open()).Resume(newCtx, "new-run", workflow.ResumeRequest{Key: fresh.Interrupts[0].Key, Input: []byte("true")})
	require.NoError(t, err)
	require.True(t, done.Done)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Len(t, done.State.Accepted, 2)
	require.Equal(t, int32(2), first.Load())
	require.Equal(t, int32(1), second.Load())
}

func TestMergeUnchangedMemberResumesWithoutRepeatingCommittedWave(t *testing.T) {
	s, c, d, first, second := mergeCompatibilityFixture(t)
	dir := t.TempDir()
	open := func() durable.Store {
		store, err := durable.NewFileStore(dir)
		require.NoError(t, err)
		return store
	}
	ctx := tools.WithSessionID(t.Context(), "stable-session")
	waiting, err := compileFixture(t, c, d, open()).Run(ctx, "stable-run", s, "")
	require.ErrorIs(t, err, agentErrors.ErrApprovalRequired)
	require.Len(t, waiting.Interrupts, 1)
	require.Len(t, waiting.State.Accepted, 1)
	done, err := compileFixture(t, c, d, open()).Resume(ctx, "stable-run", workflow.ResumeRequest{Key: waiting.Interrupts[0].Key, Input: []byte("true")})
	require.NoError(t, err)
	require.True(t, done.Done)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Equal(t, int32(1), first.Load())
	require.Equal(t, int32(1), second.Load())

	// A second process reconstruction replays the finished graph as well.
	replayed, err := compileFixture(t, c, d, open()).Run(ctx, "stable-run", s, "")
	require.NoError(t, err)
	require.Equal(t, done.State, replayed.State)
	require.Equal(t, int32(1), first.Load())
	require.Equal(t, int32(1), second.Load())
}
