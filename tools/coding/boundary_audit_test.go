package coding

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoundaryReadFileHugeLimitDoesNotOverflow(t *testing.T) {
	w := newTestWorkspace(t)
	writeFile(t, w, "input.txt", "one\ntwo\nthree\n")
	result := mustSucceed(t, w, ToolReadFile, map[string]any{
		"path": "input.txt", "offset": 2, "limit": json.Number(strconv.FormatUint(uint64(^uint(0)>>1), 10)),
	})
	require.Equal(t, 3, result["end_line"], "offset + limit overflowed instead of clamping to EOF")
	require.Contains(t, result["content"], "two")
}

func TestBoundaryGrepAcceptsSingleLineAtFileSizeLimit(t *testing.T) {
	w := newTestWorkspace(t)
	writeFile(t, w, "input.txt", "needle"+strings.Repeat("x", maxFileBytes-len("needle")))
	result := mustSucceed(t, w, ToolGrep, map[string]any{"path": "input.txt", "pattern": "needle"})
	require.Equal(t, 1, result["count"], "scanner silently omitted an accepted file at the size boundary")
}

func TestBoundaryIntegerArgumentsRejectNonFiniteAndOutOfRange(t *testing.T) {
	for _, value := range []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Ldexp(1, strconv.IntSize-1), -math.Ldexp(1, strconv.IntSize), 1e100, 1.5} {
		t.Run(strconv.FormatFloat(value, 'g', -1, 64), func(t *testing.T) {
			_, err := intArg(map[string]any{"limit": value}, "limit", 1)
			require.Error(t, err, "out-of-range float must not be converted to a machine integer")
		})
	}
}

func TestBoundaryIntegerArgumentsPreserveValidValues(t *testing.T) {
	for _, value := range []any{int(42), int64(42), float64(42), json.Number("42")} {
		got, err := intArg(map[string]any{"limit": value}, "limit", 1)
		require.NoError(t, err)
		require.Equal(t, 42, got)
	}
	minInt := -int(^uint(0)>>1) - 1
	for _, value := range []any{minInt, int64(minInt), float64(minInt), json.Number(strconv.Itoa(minInt))} {
		got, err := intArg(map[string]any{"limit": value}, "limit", 1)
		require.NoError(t, err)
		require.Equal(t, minInt, got)
	}
}
