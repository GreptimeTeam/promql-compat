package report

import (
	"testing"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

// The primary view skips only the corpus @-variants and range-mode
// self-check; every evaluation of a differential case is primary.
func TestPrimaryViewSkipsOnlyDerivedChecks(t *testing.T) {
	cases := []result.Case{
		{Suite: "compliance", Status: result.FailValue, Checks: []result.Check{
			{Kind: "range", Status: result.Pass},
			{Kind: "instant", Status: result.FailValue, Cause: result.CauseOther},
		}},
		{Suite: "corpus", Status: result.FailValue, Checks: []result.Check{
			{Kind: "instant", Status: result.Pass},
			{Kind: "range_mode", Status: result.FailValue, Cause: result.CauseOther},
			{Kind: "instant_at", Status: result.FailValue, Cause: result.CauseOther},
		}},
	}
	if got := views(cases)[1]; got.Passed != 1 || got.Total != 2 {
		t.Fatalf("primary view = %d/%d, want 1/2", got.Passed, got.Total)
	}
}
