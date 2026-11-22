package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

func TestPlanningProcessWaveCommitted(t *testing.T)          { runPlanningProcess(t, "wave") }
func TestPlanningProcessBranchCommitted(t *testing.T)        { runPlanningProcess(t, "branch") }
func TestPlanningProcessPartiallyAnsweredWaits(t *testing.T) { runPlanningProcess(t, "waits") }
func TestPlanningProcessEffectBeforeReceipt(t *testing.T)    { runPlanningProcess(t, "effect") }

func jointMarkFile(dir, name, value string) error {
	f, e := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, w := f.WriteString(value)
	return errors.Join(w, f.Sync(), f.Close())
}
func runPlanningProcess(t *testing.T, mode string) {
	if dir := os.Getenv("PLANNING_INTEGRATION_CRASH_DIR"); dir != "" {
		childPlanningProcess(t, dir, mode, os.Getenv("PLANNING_INTEGRATION_PHASE"))
		return
	}
	dir := t.TempDir()
	start := func(phase string) (*exec.Cmd, *bufio.Reader, *bytes.Buffer) {
		ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
		t.Cleanup(stop)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run", "^"+regexp.QuoteMeta(t.Name())+"$")
		cmd.Env = append(os.Environ(), "PLANNING_INTEGRATION_CRASH_DIR="+dir, "PLANNING_INTEGRATION_PHASE="+phase)
		r, e := cmd.StdoutPipe()
		require.NoError(t, e)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		return cmd, bufio.NewReader(r), &stderr
	}
	cmd, r, stderr := start("first")
	var output strings.Builder
	for {
		line, e := r.ReadString('\n')
		output.WriteString(line)
		if line == "CHECKPOINT "+mode+"\n" {
			break
		}
		if e != nil {
			_ = cmd.Wait()
			t.Fatalf("子进程未到达栅栏: %v\n%s%s", e, output.String(), stderr.String())
		}
	}
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	// 在启动恢复进程前，通过新建宿主读取磁盘提交点。
	h := openJointHost(t, dir)
	s, c, task := jointFixture(t)
	if mode == "effect" {
		c.Members[0].EffectClass = "write"
		c.Members[0].Claims = []p.ResourceClaim{{Key: "report", Mode: "write"}}
	} else {
		c.Parallelism = 2
	}
	d := jointDeps(t, h, jointPlanClient(t, task), jointReviewClient())
	g := h.graph(t, c, d)
	before, e := g.Snapshot(h.ctx, planningRoot)
	require.NoError(t, e)
	require.Equal(t, 1, before.State.Waves)
	require.NotNil(t, before.State.Wave)
	n := len(before.State.Wave.Assignments)
	wantTasks := 2
	if mode == "effect" {
		wantTasks = 1
	}
	require.Equal(t, wantTasks, n)
	effects, e := filepath.Glob(dir + "/effect-*")
	require.NoError(t, e)
	initial := map[string]int{"wave": 0, "branch": 1, "waits": 1, "effect": 1}
	require.Len(t, effects, initial[mode])
	if mode == "waits" || mode == "branch" {
		require.Len(t, before.Interrupts, 1)
	}
	if mode == "effect" {
		rec, e := durable.InspectOperation(durable.WithExecutionKey(h.ctx, before.State.Wave.Assignments[0].AttemptKey), "write-result")
		require.NoError(t, e)
		require.NotNil(t, rec)
		require.Equal(t, "pending", rec.Status)
		require.Equal(t, 1, rec.Attempts)
	}
	quota, e := h.controller.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, 1, quota.ReservedRequests)
	resumed, out, errOut := start("resume")
	var tail bytes.Buffer
	_, e = out.WriteTo(&tail)
	require.NoError(t, e)
	e = resumed.Wait()
	require.NoError(t, e, "%s%s", tail.String(), errOut.String())
	after, e := g.Snapshot(h.ctx, planningRoot)
	require.NoError(t, e)
	require.True(t, after.Done)
	require.Equal(t, "succeeded", after.State.Outcome.Status)
	require.Empty(t, after.Interrupts)
	require.Len(t, after.State.Attempts, n)
	require.Equal(t, s.Goal.ID, after.State.Goal.ID)
	for i, a := range before.State.Wave.Assignments {
		require.Equal(t, a.AttemptKey, after.State.Attempts[i].ExecutionKey)
		value, e := os.ReadFile(filepath.Join(dir, fmt.Sprintf("effect-%d", i)))
		require.NoError(t, e)
		require.Equal(t, durable.ChildKey(a.AttemptKey, "write-result"), string(value))
		calls, e := os.ReadFile(filepath.Join(dir, fmt.Sprintf("calls-%d", i)))
		require.NoError(t, e)
		require.Equal(t, "call\n", string(calls))
	}
	if mode == "effect" {
		rec, e := durable.InspectOperation(durable.WithExecutionKey(h.ctx, before.State.Wave.Assignments[0].AttemptKey), "write-result")
		require.NoError(t, e)
		require.NotNil(t, rec)
		require.True(t, rec.Reconciled)
		require.Equal(t, 1, rec.Attempts)
	}
	if mode == "branch" || mode == "waits" {
		resumedSlots, e := filepath.Glob(dir + "/resume-slot-*")
		require.NoError(t, e)
		require.Len(t, resumedSlots, 1)
	}
	final, e := h.controller.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, quota, final)
}
func childPlanningProcess(t *testing.T, dir, mode, phase string) {
	h := openJointHost(t, dir)
	s, c, task := jointFixture(t)
	tasks := []p.TaskSpec{task}
	if mode != "effect" {
		c.Parallelism = 2
		b := task
		b.ID = "new:second"
		tasks = append(tasks, b)
	} else {
		c.Members[0].EffectClass = "write"
		c.Members[0].Claims = []p.ResourceClaim{{Key: "report", Mode: "write"}}
	}
	pc := mocks.NewScriptedClient()
	if phase == "first" {
		pc = mocks.NewScriptedClient(mocks.TextReply("一次物理规划", jointJSON(t, p.PlanDraft{Tasks: tasks})))
	}
	d := jointDeps(t, h, pc, jointReviewClient())
	d.Reviewer = jointReviewer(func(_ context.Context, in p.ReviewInput) (p.Review, error) { return jointReview(in), nil })
	var once sync.Once
	barrier := func() { once.Do(func() { fmt.Println("CHECKPOINT " + mode) }); <-h.ctx.Done() }
	d.Executor = jointExecutor(func(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
		if phase == "resume" {
			if e := jointMarkFile(dir, fmt.Sprintf("resume-slot-%d", a.Slot), a.AttemptKey); e != nil {
				return p.TaskResult{}, e
			}
		}
		if mode == "wave" && phase == "first" {
			barrier()
			return p.TaskResult{}, ctx.Err()
		}
		if mode == "waits" || mode == "branch" && a.Slot == 1 {
			if _, e := durable.AwaitInput(ctx, "release", []byte(`{"prompt":"继续"}`), nil); e != nil {
				return p.TaskResult{}, e
			}
		}
		path := filepath.Join(dir, fmt.Sprintf("effect-%d", a.Slot))
		reconcile := func(ctx context.Context) (json.RawMessage, bool, error) {
			data, e := os.ReadFile(path)
			if os.IsNotExist(e) {
				return nil, false, nil
			}
			if e != nil {
				return nil, false, e
			}
			if string(data) != durable.ExecutionKey(ctx) {
				return nil, false, errors.New("外部业务去重键不匹配")
			}
			return []byte("true"), true, nil
		}
		_, e := durable.Do(ctx, "write-result", []byte("{}"), durable.OperationPolicy{MaxAttempts: 1, Reconcile: reconcile}, func(ctx context.Context) (json.RawMessage, error) {
			f, e := os.OpenFile(filepath.Join(dir, fmt.Sprintf("calls-%d", a.Slot)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				return nil, e
			}
			_, w := f.WriteString("call\n")
			if e = errors.Join(w, f.Sync(), f.Close()); e != nil {
				return nil, e
			}
			if e = jointMarkFile(dir, fmt.Sprintf("effect-%d", a.Slot), durable.ExecutionKey(ctx)); e != nil {
				return nil, e
			}
			if mode == "effect" && phase == "first" {
				barrier()
			}
			return []byte("true"), nil
		})
		return jointResult(a), e
	})
	g := h.graph(t, c, d)
	if phase == "first" {
		snap, e := g.Run(h.ctx, planningRoot, s, "")
		require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
		if mode == "waits" {
			require.Len(t, snap.Interrupts, 2)
			key := snap.Interrupts[1].Key
			snap, e = g.Resume(h.ctx, planningRoot, workflow.ResumeRequest{Key: snap.Interrupts[0].Key, Input: []byte("true")})
			require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
			require.Len(t, snap.Interrupts, 1)
			require.Equal(t, key, snap.Interrupts[0].Key)
		}
		require.Len(t, snap.Interrupts, 1)
		barrier()
		return
	}
	before, e := g.Snapshot(h.ctx, planningRoot)
	require.NoError(t, e)
	var snap *workflow.GraphSnapshot[p.State]
	if before.Waiting {
		require.Len(t, before.Interrupts, 1)
		snap, e = g.Resume(h.ctx, planningRoot, workflow.ResumeRequest{Key: before.Interrupts[0].Key, Input: []byte("true")})
	} else {
		snap, e = g.Run(h.ctx, planningRoot, s, "")
	}
	require.NoError(t, e)
	require.True(t, snap.Done)
	require.Empty(t, pc.Requests())
}
