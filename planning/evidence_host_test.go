package planning

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskResultResolvesEvidenceDigestsOnHost(t *testing.T) {
	a, r, c := assignmentFixture()
	s := SourceInput{ID: "s", Content: []byte(`{"v":9007199254740993}`)}
	var err error
	s.Digest, err = SourceDigest(s.Content)
	require.NoError(t, err)
	quote, err := SourceDigest([]byte("9007199254740993"))
	require.NoError(t, err)
	a.GoalInputs = []SourceInput{s}
	ref := EvidenceRef{SourceID: s.ID, LocatorKind: "json_pointer", Locator: "/v"}
	for _, candidate := range []EvidenceRef{ref, {SourceID: s.ID, LocatorKind: "json_pointer", Locator: "/v", SourceDigest: s.Digest}, {SourceID: s.ID, LocatorKind: "json_pointer", Locator: "/v", QuoteDigest: quote}} {
		require.Error(t, ValidateEvidence(candidate, a.GoalInputs, nil))
		r.Artifacts[0].Sources = []EvidenceRef{candidate}
		result, e := ValidateTaskResult(a, r, c)
		require.NoError(t, e)
		art := result.Artifacts[0]
		got := art.Sources[0]
		require.Equal(t, s.Digest, got.SourceDigest)
		require.Equal(t, quote, got.QuoteDigest)
		require.NoError(t, ValidateEvidence(got, a.GoalInputs, nil))
		d, e := artifactDigest(art)
		require.NoError(t, e)
		require.Equal(t, d, art.Digest)
		require.Equal(t, candidate, r.Artifacts[0].Sources[0])
	}
	for _, bad := range []EvidenceRef{{SourceID: s.ID, LocatorKind: "json_pointer", Locator: "/v", SourceDigest: "wrong"}, {SourceID: s.ID, LocatorKind: "json_pointer", Locator: "/v", QuoteDigest: "wrong"}} {
		r.Artifacts[0].Sources = []EvidenceRef{bad}
		_, err = ValidateTaskResult(a, r, c)
		require.ErrorAs(t, err, new(*CandidateError))
	}
}
