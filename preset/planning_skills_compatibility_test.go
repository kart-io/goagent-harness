package preset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

const planningSkillFrontmatter = "name: audit\ndescription: Inspect the selected report\nmetadata: {version: '1'}"
const planningSkillBody = "Read the selected report and cite its source."

func planningSkillsPreset(t *testing.T) *Resolved {
	t.Helper()
	return mustGetID(t, "lead", trusted(`presets:
- id: lead
  components:
  - {id: team, use: planning-members, config: {members: [{id: reader, preset: child}]}}
  - {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}
- id: child
  components: [{id: skills, use: skills}]`))
}

func planningSkillsMember(t *testing.T, p *Resolved, root string) (*Assembly, planning.Member) {
	t.Helper()
	child, ok := p.set.Get("child")
	require.True(t, ok)
	asm, err := Build(t.Context(), child, Host{Root: root, AgentID: "lead/team/reader"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	return asm, planning.Member{ID: "reader", Version: "1", ManifestDigest: asm.Manifest.Digest(), EffectClass: "read_only"}
}

func planningSkillsAssignment(m planning.Member) planning.Assignment {
	return planning.Assignment{
		Task:       planning.TaskSpec{ID: "inspect", MemberID: m.ID, Instruction: "Read the audit skill"},
		AttemptKey: "attempt", GoalDigest: "goal", PlanRevision: 1,
		MemberVersion: m.Version, ManifestDigest: m.ManifestDigest,
	}
}

// The factory replaces only the model. Assembly, dispatch, skills and durable
// tool replay are real; every successful member must actually load its skill.
func planningSkillsFactory(t *testing.T, opened, closed *int, wait bool) PlanningAgentFactory {
	t.Helper()
	return func(_ context.Context, asm *Assembly, _ planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		*opened++
		run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
			out, err := asm.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: ToolLoadSkill, Input: &interfaces.ToolInput{CallID: "skill", Args: map[string]any{"name": "audit"}}})
			if err != nil {
				return nil, err
			}
			require.True(t, out.Success)
			require.Equal(t, planningSkillBody, out.Result.(map[string]any)["instructions"])
			if wait {
				if _, err = durable.AwaitInput(ctx, "review", json.RawMessage(`{}`), nil); err != nil {
					return nil, err
				}
			}
			return &builder.AgentOutput{Result: `{"outcome":"produced"}`}, nil
		})
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { *closed++; return nil }}, nil
	}
}

func buildPlanningSkills(t *testing.T, p *Resolved, root string, member planning.Member, factory PlanningAgentFactory) *Assembly {
	t.Helper()
	asm, err := Build(t.Context(), p, Host{Root: root, AgentID: "lead", PlanningMembers: []planning.Member{member}, PlanningAgentFactory: factory})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	return asm
}

// A metadata-only edit must invalidate the frozen member, even when its body
// and displayed description have not changed. Body-only hashing misses this.
func TestPlanningSkillsFrontmatterChangeRejectsFrozenDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "skills")
	writeSkill(t, dir, "audit", planningSkillFrontmatter, planningSkillBody)
	p := planningSkillsPreset(t)
	_, oldMember := planningSkillsMember(t, p, root)
	opened, closed := 0, 0
	factory := planningSkillsFactory(t, &opened, &closed, false)
	old := buildPlanningSkills(t, p, root, oldMember, factory)
	writeSkill(t, dir, "audit", "name: audit\ndescription: Inspect the selected report\nmetadata: {version: '2'}", planningSkillBody)
	unexpected, err := Build(t.Context(), p, Host{Root: root, AgentID: "lead", PlanningMembers: []planning.Member{oldMember}, PlanningAgentFactory: factory})
	if unexpected != nil {
		t.Cleanup(func() { require.NoError(t, unexpected.Close()) })
	}
	require.ErrorIs(t, err, ErrManifestChanged)
	_, newMember := planningSkillsMember(t, p, root)
	next := buildPlanningSkills(t, p, root, newMember, factory)
	require.NotEqual(t, old.Manifest.Digest(), next.Manifest.Digest(), "the parent must also pin the changed member directory")
	e, err := next.PlanningExecutor()
	require.NoError(t, err)
	_, err = e.Execute(planningTestContext(t), planningSkillsAssignment(oldMember))
	require.ErrorIs(t, err, ErrManifestChanged, "the new directory must reject an old frozen assignment")
	require.Zero(t, opened)
	r, err := e.Execute(planningTestContext(t), planningSkillsAssignment(newMember))
	require.NoError(t, err)
	require.Equal(t, "produced", r.Outcome)
	require.Equal(t, 1, opened)
	require.Equal(t, opened, closed)
}

// Waiting tool calls must rebuild and validate the member before reentering
// its factory; otherwise new skill metadata can silently govern an old task.
func TestPlanningSkillsWaitReentryRejectsFrontmatterChange(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "skills")
	writeSkill(t, dir, "audit", planningSkillFrontmatter, planningSkillBody)
	p := planningSkillsPreset(t)
	_, member := planningSkillsMember(t, p, root)
	opened, closed := 0, 0
	asm := buildPlanningSkills(t, p, root, member, planningSkillsFactory(t, &opened, &closed, true))
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	ctx := planningTestContext(t)
	_, err = e.Execute(ctx, planningSkillsAssignment(member))
	waits := durable.Waits(err)
	require.Len(t, waits, 1, "%v", err)
	writeSkill(t, dir, "audit", "name: audit\ndescription: Inspect the selected report\nmetadata: {version: '2'}", planningSkillBody)
	steps := waits[0].Path
	_, err = e.Execute(durable.WithResume(ctx, steps[len(steps)-1].Key, "true"), planningSkillsAssignment(member))
	require.ErrorIs(t, err, ErrManifestChanged)
	require.Equal(t, 1, opened, "changed skills must be rejected before reopening the member")
	require.Equal(t, opened, closed)
}

// This fixture was captured from the real skills v2 assembly at d6e9b68.
// Changing only the session cannot make its pinned execution use skills v3.
func TestPlanningSkillsLegacyManifestRequiresNewExecution(t *testing.T) {
	data, err := os.ReadFile("testdata/planning-skills-v2-manifest.json")
	require.NoError(t, err)
	var legacy Manifest
	require.NoError(t, json.Unmarshal(data, &legacy))
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "audit", planningSkillFrontmatter, planningSkillBody)
	p := planningSkillsPreset(t)
	child, member := planningSkillsMember(t, p, root)
	ctx := planningTestContext(t)
	require.NoError(t, (&Assembly{Manifest: legacy}).PinManifest(ctx))
	require.ErrorIs(t, child.PinManifest(ctx), ErrManifestChanged)
	require.ErrorIs(t, child.PinManifest(tools.WithSessionID(ctx, "new-session")), ErrManifestChanged)
	newRun := durable.WithExecutionKey(ctx, "new-run")
	require.NoError(t, child.PinManifest(newRun))
	require.ErrorIs(t, child.PinManifest(ctx), ErrManifestChanged, "starting a new run must not overwrite the old pin")
	opened, closed := 0, 0
	asm := buildPlanningSkills(t, p, root, member, planningSkillsFactory(t, &opened, &closed, false))
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	r, err := e.Execute(newRun, planningSkillsAssignment(member))
	require.NoError(t, err)
	require.Equal(t, "produced", r.Outcome)
	require.Equal(t, 1, opened)
	require.Equal(t, opened, closed)
}
