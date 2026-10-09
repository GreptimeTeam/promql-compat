// Package compliance runs the query templates of prometheus/compliance
// (promql/promql-test-queries.yml, Apache-2.0) as a differential suite on the
// demo dataset.
package compliance

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/GreptimeTeam/promql-compat/internal/datagen"
	"github.com/GreptimeTeam/promql-compat/internal/diff"
	"github.com/GreptimeTeam/promql-compat/internal/par"
	"github.com/GreptimeTeam/promql-compat/internal/result"
)

//go:embed promql-test-queries.yml
var queriesYAML []byte

// variantArgs is copied from prometheus/compliance promql/testcases/expand.go.
var variantArgs = map[string][]string{
	"range":                {"1s", "15s", "1m", "5m", "15m", "1h"},
	"offset":               {"1m", "5m", "10m"},
	"simpleAggrOp":         {"sum", "avg", "max", "min", "count", "stddev", "stdvar"},
	"simpleTimeAggrOp":     {"sum", "avg", "max", "min", "count", "stddev", "stdvar", "absent", "last"},
	"topBottomOp":          {"topk", "bottomk"},
	"quantile":             {"-0.5", "0.1", "0.5", "0.75", "0.95", "0.90", "0.99", "1", "1.5"},
	"arithBinOp":           {"+", "-", "*", "/", "%", "^"},
	"compBinOp":            {"==", "!=", "<", ">", "<=", ">="},
	"binOp":                {"+", "-", "*", "/", "%", "^", "==", "!=", "<", ">", "<=", ">="},
	"simpleMathFunc":       {"abs", "ceil", "floor", "exp", "sqrt", "ln", "log2", "log10", "round"},
	"extrapolatedRateFunc": {"delta", "rate", "increase"},
	"clampFunc":            {"clamp_min", "clamp_max"},
	"instantRateFunc":      {"idelta", "irate"},
	"dateFunc":             {"day_of_month", "day_of_week", "days_in_month", "hour", "minute", "month", "year"},
	"smoothingFactor":      {"0.1", "0.5", "0.8"},
	"trendFactor":          {"0.1", "0.5", "0.8"},
}

type testCase struct {
	Query       string   `yaml:"query"`
	VariantArgs []string `yaml:"variant_args"`
}

// Template is one expanded query and the template it came from.
type Template struct {
	Template, Query string
}

// Queries expands every template into concrete queries.
func Queries() ([]Template, error) {
	var f struct {
		TestCases []testCase `yaml:"test_cases"`
	}
	if err := yaml.Unmarshal(queriesYAML, &f); err != nil {
		return nil, err
	}
	var out []Template
	for _, tc := range f.TestCases {
		qs, err := expand(tc.Query, tc.VariantArgs, map[string]string{})
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			out = append(out, Template{Template: tc.Query, Query: q})
		}
	}
	return out, nil
}

func expand(tmpl string, args []string, set map[string]string) ([]string, error) {
	if len(args) == 0 {
		t, err := template.New("q").Parse(tmpl)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, set); err != nil {
			return nil, err
		}
		return []string{b.String()}, nil
	}
	vals, ok := variantArgs[args[0]]
	if !ok {
		return nil, fmt.Errorf("unknown variant arg %q", args[0])
	}
	var out []string
	for _, v := range vals {
		set[args[0]] = v
		qs, err := expand(tmpl, args[1:], set)
		if err != nil {
			return nil, err
		}
		out = append(out, qs...)
	}
	return out, nil
}

// Evaluations covers the interesting stretches of the demo dataset: the
// :10002 outage (minute 50-55), the :10001 restart (minute 70) and the month
// boundary (minute 60), with steps both aligned and unaligned to scrapes.
func Evaluations() []diff.Query {
	s := datagen.DemoStart
	return []diff.Query{
		{Start: s.Add(40 * time.Minute), End: s.Add(110 * time.Minute), Step: 10 * time.Second},
		{Start: s.Add(45*time.Minute + 3*time.Second), End: s.Add(75 * time.Minute), Step: 17 * time.Second},
		{Start: s.Add(52*time.Minute + 30*time.Second)},
		{Start: s.Add(60 * time.Minute)},
		{Start: s.Add(70*time.Minute + 7*time.Second)},
	}
}

// Run executes the suite on db, which holds ds.
func Run(ctx context.Context, r *diff.Runner, db string, ds *datagen.Dataset, workers int) ([]result.Case, error) {
	qs, err := Queries()
	if err != nil {
		return nil, err
	}
	cases := make([]result.Case, len(qs))
	par.Do(len(qs), workers, func(i int) {
		q := qs[i]
		c := result.Case{
			Suite: "compliance",
			ID:    fmt.Sprintf("compliance:%d", i+1),
			Group: q.Template,
			Expr:  q.Query,
			Repro: fmt.Sprintf("dataset: datagen.Demo(1), %d series, %d samples, %s..%s", len(ds.Series), ds.Samples(), ds.Start.Format(time.RFC3339), ds.End.Format(time.RFC3339)),
		}
		for _, ev := range Evaluations() {
			c.Checks = append(c.Checks, r.Check(ctx, db, q.Query, ev))
		}
		c.Finish()
		cases[i] = c
	})
	return cases, ctx.Err()
}
