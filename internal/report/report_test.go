package report

import (
	"strings"
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

// The template is only exercised at run time, after the suites finish; a bad
// field reference must fail here instead. Queries come from generated and
// upstream input and must not reach the page as markup.
func TestHTMLRendersAndEscapesQueries(t *testing.T) {
	rep := Report{Target: "greptimedb", Cases: []result.Case{
		{Suite: "fuzz", Expr: "up", Status: result.Pass, Checks: []result.Check{{Kind: "instant", Status: result.Pass}}},
		{Suite: "fuzz", Expr: `label_replace(up, "a", "<script>", "", "")`, Status: result.FailError, Checks: []result.Check{
			{Kind: "instant", Query: `label_replace(up, "a", "<script>", "", "")`, Status: result.FailError, Cause: result.CauseOther, Detail: "boom"},
		}},
	}}
	page, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<script>\"") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("query text is not escaped")
	}
	if !strings.Contains(page, "50.0%") {
		t.Fatal("fuzz rate 1/2 missing")
	}
}
