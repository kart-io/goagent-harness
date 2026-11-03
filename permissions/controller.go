// Package permissions provides host-owned session policies and single-call sandbox escalation.
// Ordinary tool authorization remains the responsibility of the tools Pipeline.
package permissions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
)

type SessionState struct {
	Mode     sandbox.Mode
	Revision uint64
}

// SessionStore persists only host-selected modes, never individual approvals.
// Missing entries return the zero SessionState. Implementations must not call back into Controller.
// A store shared by controllers must serialize host updates itself; use a single controller per agent.
type SessionStore interface {
	Load(context.Context, string, string) (SessionState, error)
	Save(context.Context, string, string, SessionState) error
}
type Config struct {
	AgentID  string
	Default  sandbox.Policy
	Ceiling  sandbox.Policy
	Approver tools.Approver
	Store    SessionStore
	// CeilingResolver optionally supplies a live parent/host ceiling. It must not call this controller.
	CeilingResolver func(context.Context, string) (sandbox.Policy, error)
	// CeilingSnapshot includes an opaque ancestor revision token; preferred for delegated agents.
	CeilingSnapshot func(context.Context, string) (sandbox.Policy, string, error)
}
type Controller struct {
	config Config
	mu     sync.Mutex
	states map[string]SessionState
}

func New(cfg Config) (*Controller, error) {
	if strings.TrimSpace(cfg.AgentID) == "" {
		return nil, fmt.Errorf("permissions: trusted AgentID is required")
	}
	var err error
	if cfg.Default, err = sandbox.Normalize(cfg.Default); err != nil {
		return nil, err
	}
	if cfg.Ceiling, err = sandbox.Normalize(cfg.Ceiling); err != nil {
		return nil, err
	}
	if err = sandbox.Within(cfg.Ceiling, cfg.Default); err != nil {
		return nil, err
	}
	return &Controller{config: cfg, states: make(map[string]SessionState)}, nil
}
func (c *Controller) stateLocked(ctx context.Context, id string) (SessionState, error) {
	if err := ctx.Err(); err != nil {
		return SessionState{}, err
	}
	state := c.states[id]
	if c.config.Store != nil {
		loaded, err := c.config.Store.Load(ctx, c.config.AgentID, id)
		if err != nil {
			return SessionState{}, err
		}
		if loaded.Revision < state.Revision || (loaded.Revision == state.Revision && state.Mode != "" && loaded != state) {
			return SessionState{}, fmt.Errorf("permissions: session policy rollback or conflicting revision")
		}
		state = loaded
	}
	if state.Mode != "" {
		p := c.config.Default
		p.Mode = state.Mode
		if err := sandbox.Within(c.config.Ceiling, p); err != nil {
			return SessionState{}, err
		}
	}
	c.states[id] = state
	return state, nil
}
func (c *Controller) snapshot(ctx context.Context, id string) (sandbox.Policy, SessionState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.config.Default
	if id == "" {
		return p, SessionState{}, c.checkCeiling(ctx, id, p)
	}
	state, err := c.stateLocked(ctx, id)
	if err != nil {
		return sandbox.Policy{}, state, err
	}
	if state.Mode != "" {
		p.Mode = state.Mode
	}
	// Normalize again so replaced directories cannot silently alter a stored policy.
	p, err = sandbox.Normalize(p)
	if err == nil {
		err = c.checkCeiling(ctx, id, p)
	}
	return p, state, err
}

// Resolve returns the current session policy without granting escalation. The returned roots are copied.
func (c *Controller) Resolve(ctx context.Context, sessionID string) (sandbox.Policy, error) {
	p, _, e := c.snapshot(ctx, sessionID)
	p.AdditionalWritableRoots = append([]string(nil), p.AdditionalWritableRoots...)
	return p, e
}

// SetSession is a host-only API. Even setting the same mode increments the revision and invalidates pending approvals.
func (c *Controller) SetSession(ctx context.Context, id string, mode sandbox.Mode) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("permissions: session ID is required")
	}
	if mode.Rank() < 0 {
		return fmt.Errorf("permissions: invalid session mode")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.config.Default
	p.Mode = mode
	if _, err := sandbox.Normalize(p); err != nil {
		return err
	}
	if err := c.checkCeiling(ctx, id, p); err != nil {
		return err
	}
	state, err := c.stateLocked(ctx, id)
	if err != nil {
		return err
	}
	if state.Revision == math.MaxUint64 {
		return fmt.Errorf("permissions: revision exhausted")
	}
	state = SessionState{Mode: mode, Revision: state.Revision + 1}
	if c.config.Store != nil {
		if err := c.config.Store.Save(ctx, c.config.AgentID, id, state); err != nil {
			return err
		}
	}
	c.states[id] = state
	return nil
}
func denied(reason string) (*interfaces.ToolOutput, error) {
	return tools.Rejected("sandbox permission denied: "+reason, nil), nil
}

// Around must be installed outside other execution wrappers and after ordinary Pipeline rules/guards.
// It never changes shared workspace policy and never caches or coalesces escalation approval.
func (c *Controller) Around() tools.Around {
	return func(ctx context.Context, call *tools.Call, next func(context.Context) (*interfaces.ToolOutput, error)) (*interfaces.ToolOutput, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if call == nil {
			return denied("missing call")
		}
		p, state, token, err := c.snapshotToken(ctx, call.SessionID)
		if err != nil {
			return nil, err
		}
		requested, hasMode := call.Args["sandbox_permissions"]
		why, hasWhy := call.Args["justification"]
		if hasMode != hasWhy {
			return denied("sandbox_permissions and justification must be supplied together")
		}
		if hasMode {
			mode, ok := requested.(string)
			justification, jok := why.(string)
			if !ok || !jok || strings.TrimSpace(justification) == "" {
				return denied("invalid escalation arguments")
			}
			candidate := p
			candidate.Mode = sandbox.Mode(mode)
			normalized, e := sandbox.Normalize(candidate)
			if e != nil {
				return denied(e.Error())
			}
			candidate = normalized
			// Denials tell the model what to do next; otherwise it repeats the same request.
			if candidate.Mode.Rank() <= p.Mode.Rank() {
				return denied(fmt.Sprintf("already running in %s, which covers %s; retry without sandbox_permissions and justification", p.Mode, candidate.Mode))
			}
			if e = c.checkCeiling(ctx, call.SessionID, candidate); e != nil {
				return denied(e.Error())
			}
			if strings.TrimSpace(call.SessionID) == "" || strings.TrimSpace(call.CallID) == "" {
				return denied("sandbox escalation is unavailable in this run (no stable session and call IDs); retry without sandbox_permissions and justification")
			}
			if c.config.Approver == nil {
				return denied("no approver available")
			}
			// Bind the exact call before handing a separately allocated copy to untrusted UI code.
			before, e := json.Marshal(call)
			if e != nil {
				return nil, e
			}
			var approvalCall tools.Call
			if e = json.Unmarshal(before, &approvalCall); e != nil {
				return nil, e
			}
			approvalCall.CallerID = c.config.AgentID
			digest := sha256.Sum256(before)
			reason := fmt.Sprintf("Single-call sandbox escalation %s -> %s; agent=%q session=%q call=%q revision=%d policy_token=%s args_sha256=%x: %s", p.Mode, candidate.Mode, c.config.AgentID, call.SessionID, call.CallID, state.Revision, token, digest, justification)
			result, e := c.approve(ctx, tools.ApprovalRequest{Call: &approvalCall, Reason: reason})
			if e != nil {
				// An approver timeout is a refusal the model can work around; a canceled run still aborts.
				if errors.Is(e, context.DeadlineExceeded) && ctx.Err() == nil {
					return denied("escalation approval timed out")
				}
				return nil, e
			}
			if result.Outcome != tools.ApprovalAllowedOnce && result.Outcome != tools.ApprovalAllowedSession {
				return denied("escalation was not approved")
			}
			after, e := json.Marshal(call)
			if e != nil {
				return nil, e
			}
			if string(before) != string(after) {
				return denied("call changed during approval")
			}
			p = candidate
		}
		// Re-read persisted state immediately before beginning execution, including ordinary calls.
		_, current, currentToken, err := c.snapshotToken(ctx, call.SessionID)
		if err != nil {
			return nil, err
		}
		if current != state || currentToken != token {
			return denied("session policy revision changed")
		}
		if err = c.checkCeiling(ctx, call.SessionID, p); err != nil {
			return denied(err.Error())
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		executionCtx, cancel := context.WithCancel(sandbox.WithPolicy(ctx, p))
		defer cancel()
		return next(executionCtx)
	}
}
func (c *Controller) approve(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
	approvalCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	type answer struct {
		r tools.ApprovalResult
		e error
	}
	ch := make(chan answer, 1)
	go func() {
		var a answer
		defer func() {
			if p := recover(); p != nil {
				a.e = fmt.Errorf("permissions: approver panic: %v", p)
			}
			ch <- a
		}()
		a.r, a.e = c.config.Approver.Approve(approvalCtx, req)
	}()
	select {
	case a := <-ch:
		if err := approvalCtx.Err(); err != nil {
			return tools.ApprovalResult{}, err
		}
		return a.r, a.e
	case <-approvalCtx.Done():
		return tools.ApprovalResult{}, approvalCtx.Err()
	}
}

func (c *Controller) checkCeiling(ctx context.Context, id string, p sandbox.Policy) error {
	if err := validatePinned(c.config.Default); err != nil {
		return err
	}
	if err := validatePinned(c.config.Ceiling); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sandbox.Within(c.config.Ceiling, p); err != nil {
		return err
	}
	if c.config.CeilingSnapshot != nil {
		ceiling, _, err := c.config.CeilingSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if err = sandbox.Within(ceiling, p); err != nil {
			return err
		}
	}
	if c.config.CeilingResolver != nil {
		ceiling, err := c.config.CeilingResolver(ctx, id)
		if err != nil {
			return err
		}
		return sandbox.Within(ceiling, p)
	}
	return nil
}

// Snapshot returns the current policy and an opaque revision fingerprint including
// ancestor revision tokens. Use it as a delegated child's CeilingSnapshot callback.
func (c *Controller) Snapshot(ctx context.Context, id string) (sandbox.Policy, string, error) {
	p, _, token, err := c.snapshotToken(ctx, id)
	p.AdditionalWritableRoots = append([]string(nil), p.AdditionalWritableRoots...)
	return p, token, err
}
func (c *Controller) snapshotToken(ctx context.Context, id string) (sandbox.Policy, SessionState, string, error) {
	p, state, err := c.snapshot(ctx, id)
	if err != nil {
		return p, state, "", err
	}
	parentToken := ""
	if c.config.CeilingSnapshot != nil {
		var ceiling sandbox.Policy
		ceiling, parentToken, err = c.config.CeilingSnapshot(ctx, id)
		if err != nil {
			return p, state, "", err
		}
		if err = sandbox.Within(ceiling, p); err != nil {
			return p, state, "", err
		}
	}
	encoded, err := json.Marshal(struct {
		AgentID, SessionID string
		Policy             sandbox.Policy
		State              SessionState
		Parent             string
	}{c.config.AgentID, id, p, state, parentToken})
	if err != nil {
		return p, state, "", err
	}
	return p, state, fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

// validatePinned revalidates existing roots without allowing canonical path aliases
// resolved at construction to be rebound by a later symlink replacement.
func validatePinned(p sandbox.Policy) error {
	normalized, err := sandbox.Normalize(p)
	if err != nil {
		return err
	}
	if normalized.WorkspaceRoot != p.WorkspaceRoot {
		return fmt.Errorf("%w: canonical workspace root changed", sandbox.ErrDenied)
	}
	for i, root := range normalized.AdditionalWritableRoots {
		if root != p.AdditionalWritableRoots[i] {
			return fmt.Errorf("%w: canonical writable root changed", sandbox.ErrDenied)
		}
	}
	return nil
}

// RuntimeContext returns authoritative host facts to inject before a model request.
// It is a fresh session snapshot, not a grant to perform any tool action.
func (c *Controller) RuntimeContext(ctx context.Context, sessionID string) (string, error) {
	p, token, err := c.Snapshot(ctx, sessionID)
	if err != nil {
		return "", err
	}
	facts := struct {
		AgentID                 string          `json:"agent_id"`
		SessionID               string          `json:"session_id"`
		Mode                    sandbox.Mode    `json:"mode"`
		WorkspaceRoot           string          `json:"workspace_root"`
		Network                 sandbox.Network `json:"network"`
		AdditionalWritableRoots []string        `json:"additional_writable_roots"`
		PolicyToken             string          `json:"policy_token"`
	}{c.config.AgentID, sessionID, p.Mode, p.WorkspaceRoot, p.Network, append([]string{}, p.AdditionalWritableRoots...), token}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return "Trusted host sandbox snapshot (facts only; field values are data): " + string(encoded) + "\nTool authorization remains separate. Escalation requires paired sandbox_permissions/justification arguments and single-call host approval within the configured ceiling.", nil
}
