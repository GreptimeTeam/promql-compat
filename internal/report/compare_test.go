package report

import (
	"strings"
	"testing"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

func caseOf(id, expr string, st result.Status, tags ...string) result.Case {
	return result.Case{Suite: "fuzz", ID: id, Expr: expr, Status: st, Tags: tags,
		Checks: []result.Check{{Kind: "instant", Query: expr, Status: st, Cause: result.CauseOther}}}
}

// -compare's exit status and the comparison page both rest on this
// classification; a case only counts as fixed or regressed when it is in
// scope in both runs and matches by ID and expression.
func TestCompareClassifiesCases(t *testing.T) {
	base := &Report{Cases: []result.Case{
		caseOf("1", "a", result.FailValue),
		caseOf("2", "b", result.Pass),
		caseOf("3", "c", result.FailValue),
		caseOf("4", "d", result.FailValue),
		caseOf("5", "e", result.Pass),
		caseOf("6", "f", result.FailValue),
	}}
	cur := &Report{Cases: []result.Case{
		caseOf("1", "a", result.Pass),                         // fixed
		caseOf("2", "b", result.FailError),                    // regressed
		caseOf("3", "c", result.FailValue),                    // still failing
		caseOf("4", "d", result.Pass, result.TagExperimental), // scope changed
		caseOf("5", "e2", result.Pass),                        // new expression under an old ID
		caseOf("6", "f", result.PassErrorMessage),             // fixed: counts as a pass
		caseOf("7", "g", result.FailValue),                    // only in the new run
	}}
	cmp := Compare(base, cur)
	want := SuiteChanges{Suite: "fuzz", Fixed: 2, Regressed: 1, OnlyBase: 1, OnlyNew: 2, ScopeChanged: 1}
	if len(cmp.Suites) != 1 || cmp.Suites[0] != want {
		t.Fatalf("got %+v, want %+v", cmp.Suites, want)
	}
	if len(cmp.Regressed) != 1 || cmp.Regressed[0].ID != "2" {
		t.Fatalf("regressed = %v", cmp.Regressed)
	}
}

// A run can lack a suite (VictoriaMetrics has no corpus); the page must still
// render, and query text must reach it escaped.
func TestCompareHTMLRendersRunsWithDifferentSuites(t *testing.T) {
	expr := `label_replace(up, "a", "<script>", "", "")`
	corpus := caseOf("c1", "up", result.Pass)
	corpus.Suite = "corpus"
	older := &Report{Target: "greptimedb", Label: "old", Cases: []result.Case{corpus, caseOf("1", expr, result.Pass)}}
	newer := &Report{Target: "victoriametrics", Label: "new", Cases: []result.Case{caseOf("1", expr, result.FailValue)}}
	page, err := CompareHTML([]*Report{older, newer})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<script>\"") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("query text is not escaped")
	}
	if !strings.Contains(page, "Regressed cases (1)") {
		t.Fatal("regression missing")
	}
}

// First and last labels extend inward from the plot edges, so long names
// collide even with only two runs.
func TestXLabelsDoNotOverlap(t *testing.T) {
	x := func(n int) func(int) float64 {
		return func(i int) float64 { return plotLeft + (plotRight-plotLeft)*float64(i)/float64(n-1) }
	}
	for _, names := range [][]string{
		{"GreptimeDB v1.2.1 (abcdef0)", "GreptimeDB v1.3.0 (1234567)"},
		{"v1.3.0-beta.1", "v1.3.0-beta.2", "v1.3.0-beta.3", "v1.3.0-beta.4", "v1.3.0-beta.5", "v1.3.0-beta.6", "v1.3.0-beta.7", "v1.3.0"},
	} {
		got := xLabels(names, x(len(names)))
		if got[len(got)-1].Text != names[len(names)-1] {
			t.Fatalf("%v: last run is not labelled", names)
		}
		for i := 1; i < len(got); i++ {
			_, prevHi := labelExtent(got[i-1])
			if lo, _ := labelExtent(got[i]); lo < prevHi+labelGap {
				t.Fatalf("%v: %q and %q overlap", names, got[i-1].Text, got[i].Text)
			}
		}
	}
	if got := xLabels([]string{"v1.2.1", "v1.3.0"}, x(2)); len(got) != 2 {
		t.Fatalf("short names: got %d labels, want 2", len(got))
	}
}
