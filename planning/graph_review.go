package planning

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/core/durable"
)

type recordedReview struct {
	Review   Review `json:"review"`
	Rejected string `json:"rejected"`
}

func (f planningGraph) recordReview(ctx context.Context, in ReviewInput, round int) (Review, error) {
	// Validate against an independent host copy even when the reviewer mutates its input.
	raw, e := json.Marshal(in)
	if e != nil {
		return Review{}, e
	}
	if len(raw) > f.cfg.MaxStateBytes {
		return Review{}, invalid("审查输入超出状态大小上限")
	}
	run := func(ctx context.Context) (json.RawMessage, error) {
		var copy ReviewInput
		if e := json.Unmarshal(raw, &copy); e != nil {
			return nil, e
		}
		r, e := f.deps.Reviewer.Review(ctx, copy)
		if e == nil {
			e = withinStateLimit(r, f.cfg)
			if e == nil {
				r, e = ValidateReview(in, r)
			}
		}
		out := recordedReview{}
		if e != nil {
			var bad *CandidateError
			if !errors.As(e, &bad) {
				return nil, e
			}
			out.Rejected = boundedText(e.Error())
			if out.Rejected == "" {
				out.Rejected = "审查候选无效"
			}
		} else {
			r.ID = durable.ChildKey(in.SubjectDigest, "review/"+strconv.Itoa(round))
			out.Review = r
		}
		return json.Marshal(out)
	}
	var b json.RawMessage
	if durable.HasJournal(ctx) {
		rec, e := durable.Do(ctx, "task-review-decision", raw, durable.OperationPolicy{MaxAttempts: 3, Resumable: true}, run)
		if e != nil {
			return Review{}, e
		}
		b = rec.Value
	} else {
		b, e = run(ctx)
		if e != nil {
			return Review{}, e
		}
	}
	var out recordedReview
	if e = json.Unmarshal(b, &out); e != nil {
		return Review{}, e
	}
	if out.Rejected != "" {
		return Review{}, invalid("%s", out.Rejected)
	}
	return out.Review, nil
}

func (f planningGraph) finalReview(ctx context.Context, s State) (State, error) {
	accepted, e := currentAccepted(s, f.cfg)
	if e != nil {
		return s, e
	}
	if len(accepted) != len(s.Plan.Tasks) {
		return s, fmt.Errorf("最终审查缺少已验收任务")
	}
	identities := []sourceIdentity{}
	ids := []string{}
	for _, a := range ordered(s.Accepted, func(a Artifact) string { return a.ID }) {
		identities = append(identities, sourceIdentity{a.ID, a.Digest})
		ids = append(ids, a.ID)
	}
	d, e := digestJSON(struct {
		Goal      string
		Revision  int
		Artifacts []sourceIdentity
	}{s.GoalDigest, s.Plan.Revision, identities})
	if e != nil {
		return s, e
	}
	in := ReviewInput{SubjectDigest: d, Final: true, Goal: s.Goal.clone(), Plan: s.Plan.clone(), Tasks: cloneSlice(s.Plan.Tasks, TaskSpec.clone), Accepted: cloneSlice(s.Accepted, Artifact.clone)}
	for _, t := range s.Plan.Tasks {
		in.Results = append(in.Results, accepted[t.ID].Result.clone())
	}
	r, e := f.recordReview(ctx, in, s.Plan.Revision)
	if e != nil {
		return rejectCandidate(s, e)
	}
	s.Feedback = append(s.Feedback, r)
	switch r.Verdict {
	case "accept":
		s.Outcome = &Outcome{Status: "succeeded", Reason: r.Reason, ArtifactIDs: ids}
		s.PendingAction = "finish"
	case "block":
		return blocked(s, r.Reason), nil
	default:
		s.ValidationError = r.Reason
		s.PendingAction = "revise"
	}
	return s, nil
}
