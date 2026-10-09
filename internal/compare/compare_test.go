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
			st, detail := Values(tc.ref, tc.got, tc.ordered, Epsilon)
			if st.Passed() != tc.pass {
				t.Fatalf("status %s (%s), want pass=%v", st, detail, tc.pass)
			}
		})
	}
}

// Differences measured between Prometheus and real targets: semantic ones
// that 1e-6 used to hide must fail, regression noise must pass.
func TestToleranceSeparatesMeasuredDifferences(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		ref, got   float64
		pass       bool
	}{
		{"timestamp 150s off", "timestamp(demo_num_cpus)", 1772322750, 1772322600, false},
		{"x + -(1) evaluated as x + 1", "demo_memory_usage_bytes + -(1)", 799999999, 800000001, false},
		{"deriv regression noise", "deriv(demo_batch_last_success_timestamp_seconds[10m])", 0.9956767206480017, 0.9956766794907801, true},
		{"stdvar_over_time noise", "stdvar_over_time(demo_memory_usage_bytes[1m])", 33272257359.175594, 33272257359.303665, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := parser.NewParser(parser.Options{}).ParseExpr(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			ref := promql.Vector{sample("m", "a", 1000, tc.ref)}
			got := promql.Vector{sample("m", "a", 1000, tc.got)}
			if st, detail := Values(ref, got, false, Tolerance(e)); st.Passed() != tc.pass {
				t.Fatalf("status %s (%s), want pass=%v", st, detail, tc.pass)
			}
		})
	}
}

func TestToleranceClasses(t *testing.T) {
	for expr, want := range map[string]float64{
		"timestamp(m)":                     Epsilon,
		"rate(m[5m])":                      Epsilon,
		"sum by (job) (m)":                 Epsilon,
		"deriv(m[5m])":                     IllConditionedEpsilon,
		"predict_linear(m[5m], 60)":        IllConditionedEpsilon,
		"stddev(m)":                        IllConditionedEpsilon,
		"stdvar_over_time(m[1m]) + time()": IllConditionedEpsilon,
		"atan(sinh(deriv(m[10m])))":        IllConditionedEpsilon,
	} {
		e, err := parser.NewParser(parser.Options{}).ParseExpr(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := Tolerance(e); got != want {
			t.Errorf("Tolerance(%s) = %g, want %g", expr, got, want)
		}
	}
}
