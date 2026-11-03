package permissions

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/tools/policy"
)

func fixture(t *testing.T, approver tools.Approver, store SessionStore) *Controller {
	t.Helper()
	root := t.TempDir()
	c, e := New(Config{AgentID: "trusted", Default: sandbox.Policy{Mode: sandbox.ReadOnly, WorkspaceRoot: root, Network: sandbox.NetworkDeny}, Ceiling: sandbox.Policy{Mode: sandbox.WorkspaceWrite, WorkspaceRoot: root, Network: sandbox.NetworkDeny}, Approver: approver, Store: store})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func escalation() *tools.Call {
	return &tools.Call{Tool: "write_file", CallID: "call", SessionID: "s", CallerID: "untrusted", Args: map[string]any{"path": "a", "sandbox_permissions": "workspace-write", "justification": "write requested file", "agent_id": "spoof"}}
}
func TestEscalationIsPerCallAndTrusted(t *testing.T) {
	var n, executed int
	c := fixture(t, tools.ApproverFunc(func(ctx context.Context, r tools.ApprovalRequest) (tools.ApprovalResult, error) {
		n++
		if executed != n-1 {
			t.Fatal("side effect before approval")
		}
		if r.Call.CallerID != "trusted" || r.Call.SessionID != "s" || r.Call.CallID != "call" || r.Grant != nil {
			t.Fatalf("bad identity/grant: %+v", r)
		}
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedSession}, nil
	}), nil)
	for range 2 {
		_, e := c.Around()(context.Background(), escalation(), func(ctx context.Context) (*interfaces.ToolOutput, error) {
			p, ok := sandbox.FromContext(ctx)
			if !ok || p.Mode != sandbox.WorkspaceWrite {
				t.Fatal(p)
			}
			executed++
			return &interfaces.ToolOutput{}, nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	if n != 2 {
		t.Fatal(n)
	}
	_, e := c.Around()(context.Background(), &tools.Call{SessionID: "s"}, func(ctx context.Context) (*interfaces.ToolOutput, error) {
		p, _ := sandbox.FromContext(ctx)
		if p.Mode != sandbox.ReadOnly {
			t.Fatal("escalation persisted")
		}
		return nil, nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
func TestInvalidAndDeniedNeverExecute(t *testing.T) {
	for _, kind := range []string{"denied", "pair", "same", "ceiling", "identity", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
				return tools.ApprovalResult{Outcome: tools.ApprovalRejected}, nil
			}), nil)
			call := escalation()
			ctx := context.Background()
			switch kind {
			case "pair":
				delete(call.Args, "justification")
			case "same":
				call.Args["sandbox_permissions"] = "read-only"
			case "ceiling":
				call.Args["sandbox_permissions"] = "danger-full-access"
			case "identity":
				call.SessionID = ""
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			ran := false
			_, _ = c.Around()(ctx, call, func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
			if ran {
				t.Fatal("executed")
			}
		})
	}
}
func TestConcurrentApprovalIsolationAndRevision(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var asked, ran atomic.Int32
	c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		asked.Add(1)
		entered <- struct{}{}
		<-release
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Around()(context.Background(), escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran.Add(1); return nil, nil })
		}()
	}
	<-entered
	<-entered
	if e := c.SetSession(context.Background(), "s", sandbox.ReadOnly); e != nil {
		t.Fatal(e)
	}
	close(release)
	wg.Wait()
	if asked.Load() != 2 || ran.Load() != 0 {
		t.Fatalf("asked %d ran %d", asked.Load(), ran.Load())
	}
}

type memoryStore struct {
	mu     sync.Mutex
	states map[string]SessionState
}

func (s *memoryStore) Load(_ context.Context, a, id string) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[a+"/"+id], nil
}
func (s *memoryStore) Save(_ context.Context, a, id string, v SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[a+"/"+id] = v
	return nil
}
func TestSessionRecoveryAndCeiling(t *testing.T) {
	s := &memoryStore{states: map[string]SessionState{}}
	c := fixture(t, nil, s)
	if e := c.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); e != nil {
		t.Fatal(e)
	}
	recovered := fixture(t, nil, s)
	_, e := recovered.Around()(context.Background(), &tools.Call{SessionID: "s"}, func(ctx context.Context) (*interfaces.ToolOutput, error) {
		p, _ := sandbox.FromContext(ctx)
		if p.Mode != sandbox.WorkspaceWrite {
			t.Fatal(p)
		}
		return nil, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if e := c.SetSession(context.Background(), "s", sandbox.DangerFullAccess); e == nil {
		t.Fatal("ceiling bypass")
	}
	if s.states["trusted/s"].Revision != 1 {
		t.Fatal(s.states)
	}
}
func TestRoleRulesNoShellPrivilege(t *testing.T) {
	for _, role := range []string{"reader", "reviewer", "coordinator"} {
		r, e := RoleRules(role)
		if e != nil {
			t.Fatal(e)
		}
		p := policy.Policy{Rules: r, Default: policy.Ask}
		for _, tool := range []string{"run_command", "git_commit", "write_file"} {
			if p.Decide(&tools.Call{Tool: tool}) != policy.Deny {
				t.Fatalf("%s allows %s", role, tool)
			}
		}
	}
	r, _ := RoleRules("coder")
	p := policy.Policy{Rules: r, Default: policy.Ask}
	if p.Decide(&tools.Call{Tool: "run_command", Args: map[string]any{"command": "rm -rf /"}}) != policy.Ask {
		t.Fatal("coder auto shell")
	}
	if _, e := RoleRules("unknown"); e == nil {
		t.Fatal("unknown role")
	}
}
func TestDynamicCeilingBlocksBeforeApproval(t *testing.T) {
	var asked int
	c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		asked++
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	c.config.CeilingResolver = func(context.Context, string) (sandbox.Policy, error) { return c.config.Default, nil }
	ran := false
	_, _ = c.Around()(context.Background(), escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
	if asked != 0 || ran {
		t.Fatal("dynamic ceiling exceeded")
	}
}
func TestDynamicCeilingRecheckedAfterApproval(t *testing.T) {
	var narrowed bool
	c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		narrowed = true
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	c.config.CeilingResolver = func(context.Context, string) (sandbox.Policy, error) {
		if narrowed {
			return c.config.Default, nil
		}
		return c.config.Ceiling, nil
	}
	ran := false
	_, _ = c.Around()(context.Background(), escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
	if ran {
		t.Fatal("stale parent approval")
	}
}
func TestCancellationIgnoresLateApproval(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		close(entered)
		<-release
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var ran atomic.Bool
	go func() {
		defer close(done)
		_, _ = c.Around()(ctx, escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran.Store(true); return nil, nil })
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval blocked cancellation")
	}
	close(release)
	if ran.Load() {
		t.Fatal("canceled execution")
	}
}
func TestStoreRollbackAndIdentitySeparation(t *testing.T) {
	store := &memoryStore{states: map[string]SessionState{}}
	c := fixture(t, nil, store)
	if e := c.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); e != nil {
		t.Fatal(e)
	}
	p, e := c.Resolve(context.Background(), "other")
	if e != nil || p.Mode != sandbox.ReadOnly {
		t.Fatal("session state leaked", p, e)
	}
	store.mu.Lock()
	store.states["trusted/s"] = SessionState{}
	store.mu.Unlock()
	if _, e = c.Resolve(context.Background(), "s"); e == nil {
		t.Fatal("rollback accepted")
	}
}
func TestConstructionRequiresTrustedIdentity(t *testing.T) {
	root := t.TempDir()
	p := sandbox.Policy{WorkspaceRoot: root}
	if _, e := New(Config{Default: p, Ceiling: p}); e == nil {
		t.Fatal("missing AgentID accepted")
	}
}
func TestParentRevisionAwayAndBackInvalidatesChild(t *testing.T) {
	parent := fixture(t, nil, nil)
	child := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		if e := parent.SetSession(context.Background(), "s", sandbox.ReadOnly); e != nil {
			t.Fatal(e)
		}
		if e := parent.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); e != nil {
			t.Fatal(e)
		}
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	// Give parent the same workspace so full containment, not just mode rank, applies.
	parent.config.Default.WorkspaceRoot = child.config.Default.WorkspaceRoot
	parent.config.Ceiling.WorkspaceRoot = child.config.Ceiling.WorkspaceRoot
	if e := parent.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); e != nil {
		t.Fatal(e)
	}
	child.config.CeilingSnapshot = parent.Snapshot
	ran := false
	_, _ = child.Around()(context.Background(), escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
	if ran {
		t.Fatal("ancestor revision change reused approval")
	}
}
func TestCanonicalWorkspaceReplacementDenied(t *testing.T) {
	c := fixture(t, nil, nil)
	root := c.config.Default.WorkspaceRoot
	outside := t.TempDir()
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(context.Background(), "s"); err == nil {
		t.Fatal("rebound canonical workspace")
	}
	ran := false
	_, _ = c.Around()(context.Background(), &tools.Call{SessionID: "s"}, func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
	if ran {
		t.Fatal("executed after workspace replacement")
	}
}
func TestRuntimeContextUsesCurrentHostPolicy(t *testing.T) {
	c := fixture(t, nil, nil)
	before, err := c.RuntimeContext(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "read-only") || !strings.Contains(before, c.config.Default.WorkspaceRoot) || !strings.Contains(before, "deny") {
		t.Fatal(before)
	}
	if err = c.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); err != nil {
		t.Fatal(err)
	}
	after, err := c.RuntimeContext(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "workspace-write") || after == before {
		t.Fatal(after)
	}
}
func TestSetSessionRespectsLiveParentAndPinnedRoots(t *testing.T) {
	t.Run("parent", func(t *testing.T) {
		store := &memoryStore{states: map[string]SessionState{}}
		child := fixture(t, nil, store)
		child.config.CeilingSnapshot = func(context.Context, string) (sandbox.Policy, string, error) {
			return child.config.Default, "parent-readonly", nil
		}
		if err := child.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); err == nil {
			t.Fatal("host switch exceeded live parent")
		}
		if len(store.states) != 0 {
			t.Fatal("denied switch was persisted")
		}
	})
	t.Run("pinned", func(t *testing.T) {
		store := &memoryStore{states: map[string]SessionState{}}
		c := fixture(t, nil, store)
		root := c.config.Default.WorkspaceRoot
		outside := t.TempDir()
		if err := os.Rename(root, root+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		if err := c.SetSession(context.Background(), "s", sandbox.WorkspaceWrite); err == nil {
			t.Fatal("host switch rebound workspace")
		}
		if len(store.states) != 0 {
			t.Fatal("denied switch was persisted")
		}
	})
}

// 审批超时按拒绝交给模型，运行继续；父 ctx 结束时仍返回错误。
func TestApprovalTimeoutIsDenial(t *testing.T) {
	c := fixture(t, tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		return tools.ApprovalResult{}, context.DeadlineExceeded
	}), nil)
	ran := false
	out, err := c.Around()(context.Background(), escalation(), func(context.Context) (*interfaces.ToolOutput, error) { ran = true; return nil, nil })
	if err != nil || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	if msg, ok := tools.RejectedResult(out); !ok || !strings.Contains(msg, "timed out") {
		t.Fatalf("want timeout denial, got %#v", out)
	}
}
