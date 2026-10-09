package compare

import (
	"math"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
)

func sample(name, inst string, t int64, f float64) promql.Sample {
	return promql.Sample{Metric: labels.FromStrings("__name__", name, "instance", inst), T: t, F: f}
}

func series(inst string, pts ...promql.FPoint) promql.Series {
	return promql.Series{Metric: labels.FromStrings("__name__", "m", "instance", inst), Floats: pts}
}

// The comparator decides every measured rate: a wrong answer must fail, and
// differences Prometheus itself does not guarantee must pass.
func TestValuesSemantics(t *testing.T) {
	vec := promql.Vector{sample("m", "a", 1000, 1), sample("m", "b", 1000, 2)}
	mat := promql.Matrix{
		series("a", promql.FPoint{T: 0, F: 1}, promql.FPoint{T: 1000, F: 2}),
		series("b", promql.FPoint{T: 0, F: 3}),
	}
	nan := promql.Vector{sample("m", "a", 1000, math.NaN())}

	for _, tc := range []struct {
		name     string
		ref, got parser.Value
		ordered  bool
		pass     bool
	}{
		{"changed value", vec, promql.Vector{sample("m", "a", 1000, 1), sample("m", "b", 1000, 2.5)}, false, false},
		{"missing series", vec, vec[:1], false, false},
		{"extra series", vec[:1], vec, false, false},
		{"changed label", vec, promql.Vector{sample("m", "a", 1000, 1), sample("m", "c", 1000, 2)}, false, false},
		{"shifted sample time", vec, promql.Vector{sample("m", "a", 1000, 1), sample("m", "b", 2000, 2)}, false, false},
		{"NaN replaced by number", nan, promql.Vector{sample("m", "a", 1000, 0)}, false, false},
		{"order of sort()", vec, promql.Vector{vec[1], vec[0]}, true, false},
		{"changed scalar", promql.Scalar{T: 1000, V: 1}, promql.Scalar{T: 1000, V: 2}, false, false},
		{"type mismatch", promql.Scalar{T: 1000, V: 1}, promql.Vector{sample("m", "a", 1000, 1)}, false, false},
		{"shifted matrix point", mat, promql.Matrix{
			series("a", promql.FPoint{T: 0, F: 1}, promql.FPoint{T: 2000, F: 2}),
			mat[1],
		}, false, false},
		{"changed matrix point", mat, promql.Matrix{
			series("a", promql.FPoint{T: 0, F: 1}, promql.FPoint{T: 1000, F: 2.1}),
			mat[1],
		}, false, false},

		{"identical", vec, vec, false, true},
		{"NaN equals NaN", nan, nan, false, true},
		{"float rounding", vec, promql.Vector{sample("m", "a", 1000, 1+1e-12), sample("m", "b", 1000, 2)}, false, true},
		{"vector order without sort()", vec, promql.Vector{vec[1], vec[0]}, false, true},
		{"matrix series order", mat, promql.Matrix{mat[1], mat[0]}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, detail := Values(tc.ref, tc.got, tc.ordered)
			if st.Passed() != tc.pass {
				t.Fatalf("status %s (%s), want pass=%v", st, detail, tc.pass)
			}
		})
	}
}
