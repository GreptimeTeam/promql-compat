// Package compare diffs two PromQL results, the reference and the target.
package compare

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/util/almost"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

const (
	// Epsilon is the relative tolerance for well-conditioned evaluation.
	// Measured against GreptimeDB and VictoriaMetrics, such queries agree
	// within 1e-12, while semantic differences hidden by 1e-6 start at 1.6e-9
	// (an off-by-two on 8e8, a 150 s offset on a timestamp).
	Epsilon = 1e-10
	// IllConditionedEpsilon is for queries using least-squares regression or
	// variance: equivalent implementations differ there up to 4e-8, on both
	// sides of the exact value.
	IllConditionedEpsilon = 1e-6
)

var illConditioned = map[string]bool{
	"deriv": true, "predict_linear": true, "stddev_over_time": true, "stdvar_over_time": true,
}

// Tolerance returns the relative tolerance for comparing results of expr:
// IllConditionedEpsilon if any part of it is ill-conditioned, else Epsilon.
func Tolerance(expr parser.Expr) float64 {
	eps := Epsilon
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch x := n.(type) {
		case *parser.Call:
			if illConditioned[x.Func.Name] {
				eps = IllConditionedEpsilon
			}
		case *parser.AggregateExpr:
			if x.Op == parser.STDDEV || x.Op == parser.STDVAR {
				eps = IllConditionedEpsilon
			}
		}
		return nil
	})
	return eps
}

// noiseFloor: two results both smaller than this are treated as equal. They
// come from catastrophic cancellation (stdvar of near-equal values), where
// even two Prometheus runs disagree in the leading digit.
const noiseFloor = 1e-10

func floatEqual(a, b, eps float64) bool {
	if almost.Equal(a, b, eps) {
		return true
	}
	return math.Abs(a) < noiseFloor && math.Abs(b) < noiseFloor
}

const maxDiffs = 5

// Values compares got against ref with relative tolerance eps. ordered
// requires instant vectors to match in order (sort(), sort_desc() and friends
// at the top level).
func Values(ref, got parser.Value, ordered bool, eps float64) (result.Status, string) {
	if ref.Type() != got.Type() {
		return result.FailValue, fmt.Sprintf("result type %s, want %s", got.Type(), ref.Type())
	}
	switch r := ref.(type) {
	case promql.Scalar:
		g := got.(promql.Scalar)
		if r.T != g.T || !floatEqual(r.V, g.V, eps) {
			return result.FailValue, fmt.Sprintf("scalar %v@%d, want %v@%d", g.V, g.T, r.V, r.T)
		}
	case promql.String:
		g := got.(promql.String)
		if r.V != g.V {
			return result.FailValue, fmt.Sprintf("string %q, want %q", g.V, r.V)
		}
	case promql.Vector:
		return vectors(r, got.(promql.Vector), ordered, eps)
	case promql.Matrix:
		return matrices(r, got.(promql.Matrix), eps)
	}
	return result.Pass, ""
}

func vectors(ref, got promql.Vector, ordered bool, eps float64) (result.Status, string) {
	var diffs []string
	if ordered {
		if len(ref) != len(got) {
			diffs = append(diffs, fmt.Sprintf("%d samples, want %d", len(got), len(ref)))
		}
		for i := 0; i < len(ref) && i < len(got) && len(diffs) < maxDiffs; i++ {
			if !labels.Equal(ref[i].Metric, got[i].Metric) || !sampleEqual(ref[i], got[i], eps) {
				diffs = append(diffs, fmt.Sprintf("position %d: got %s, want %s", i, fmtSample(got[i]), fmtSample(ref[i])))
			}
		}
		return verdict(diffs)
	}
	gm := map[uint64]promql.Sample{}
	for _, s := range got {
		if _, dup := gm[s.Metric.Hash()]; dup {
			diffs = append(diffs, "duplicate series "+s.Metric.String())
		}
		gm[s.Metric.Hash()] = s
	}
	seen := map[uint64]bool{}
	for _, r := range ref {
		h := r.Metric.Hash()
		seen[h] = true
		g, ok := gm[h]
		switch {
		case !ok:
			diffs = append(diffs, "missing "+fmtSample(r))
		case !sampleEqual(r, g, eps):
			diffs = append(diffs, fmt.Sprintf("got %s, want %s", fmtSample(g), fmtSample(r)))
		}
	}
	for _, g := range got {
		if !seen[g.Metric.Hash()] {
			diffs = append(diffs, "unexpected "+fmtSample(g))
		}
	}
	return verdict(diffs)
}

func sampleEqual(a, b promql.Sample, eps float64) bool {
	if a.T != b.T {
		return false
	}
	if (a.H == nil) != (b.H == nil) {
		return false
	}
	if a.H != nil {
		return a.H.Equals(b.H)
	}
	return floatEqual(a.F, b.F, eps)
}

func fmtSample(s promql.Sample) string {
	if s.H != nil {
		return fmt.Sprintf("%s => histogram@%d", s.Metric, s.T)
	}
	return fmt.Sprintf("%s => %v@%d", s.Metric, s.F, s.T)
}

func matrices(ref, got promql.Matrix, eps float64) (result.Status, string) {
	status := result.Pass
	detail := ""
	if !sort.IsSorted(got) {
		status, detail = result.PassUnsorted, "matrix not sorted by labels"
		got = append(promql.Matrix(nil), got...)
		sort.Sort(got)
	}
	ref = append(promql.Matrix(nil), ref...)
	sort.Sort(ref)
	var diffs []string
	gm := map[uint64]promql.Series{}
	for _, s := range got {
		if _, dup := gm[s.Metric.Hash()]; dup {
			diffs = append(diffs, "duplicate series "+s.Metric.String())
		}
		gm[s.Metric.Hash()] = s
	}
	seen := map[uint64]bool{}
	for _, r := range ref {
		h := r.Metric.Hash()
		seen[h] = true
		g, ok := gm[h]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing series %s (%d points, first %s)", r.Metric, len(r.Floats), firstPoint(r)))
			continue
		}
		if d := seriesDiff(r, g, eps); d != "" {
			diffs = append(diffs, r.Metric.String()+": "+d)
		}
	}
	for _, g := range got {
		if !seen[g.Metric.Hash()] {
			diffs = append(diffs, fmt.Sprintf("unexpected series %s (%d points, first %s)", g.Metric, len(g.Floats), firstPoint(g)))
		}
	}
	if len(diffs) > 0 {
		return verdict(diffs)
	}
	return status, detail
}

func firstPoint(s promql.Series) string {
	if len(s.Floats) == 0 {
		return "-"
	}
	return fmt.Sprintf("%v@%d", s.Floats[0].F, s.Floats[0].T)
}

// seriesDiff describes the first differences between two float series.
func seriesDiff(ref, got promql.Series, eps float64) string {
	rm := map[int64]float64{}
	for _, p := range ref.Floats {
		rm[p.T] = p.F
	}
	gm := map[int64]float64{}
	for _, p := range got.Floats {
		gm[p.T] = p.F
	}
	var diffs []string
	n := 0
	for _, p := range ref.Floats {
		g, ok := gm[p.T]
		switch {
		case !ok:
			n++
			if len(diffs) < 3 {
				diffs = append(diffs, fmt.Sprintf("missing point %v@%d", p.F, p.T))
			}
		case !floatEqual(p.F, g, eps):
			n++
			if len(diffs) < 3 {
				diffs = append(diffs, fmt.Sprintf("@%d got %v want %v", p.T, g, p.F))
			}
		}
	}
	for _, p := range got.Floats {
		if _, ok := rm[p.T]; !ok {
			n++
			if len(diffs) < 3 {
				diffs = append(diffs, fmt.Sprintf("extra point %v@%d", p.F, p.T))
			}
		}
	}
	if len(ref.Histograms) != len(got.Histograms) {
		n++
		diffs = append(diffs, fmt.Sprintf("%d histogram points, want %d", len(got.Histograms), len(ref.Histograms)))
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d point diffs of %d: %s", n, len(ref.Floats), strings.Join(diffs, "; "))
}

func verdict(diffs []string) (result.Status, string) {
	if len(diffs) == 0 {
		return result.Pass, ""
	}
	extra := ""
	if len(diffs) > maxDiffs {
		extra = fmt.Sprintf(" (+%d more)", len(diffs)-maxDiffs)
		diffs = diffs[:maxDiffs]
	}
	return result.FailValue, strings.Join(diffs, " | ") + extra
}

// Ordered reports whether the top-level expression defines result order.
func Ordered(expr parser.Expr) bool {
	for {
		switch e := expr.(type) {
		case *parser.ParenExpr:
			expr = e.Expr
		case *parser.StepInvariantExpr:
			expr = e.Expr
		case *parser.Call:
			switch e.Func.Name {
			case "sort", "sort_desc", "sort_by_label", "sort_by_label_desc":
				return true
			}
			return false
		default:
			return false
		}
	}
}

// DropName removes __name__ from every series of v.
func DropName(v parser.Value) parser.Value {
	switch t := v.(type) {
	case promql.Vector:
		out := make(promql.Vector, len(t))
		for i, s := range t {
			s.Metric = s.Metric.DropMetricName()
			out[i] = s
		}
		return out
	case promql.Matrix:
		out := make(promql.Matrix, len(t))
		for i, s := range t {
			s.Metric = s.Metric.DropMetricName()
			out[i] = s
		}
		return out
	}
	return v
}
