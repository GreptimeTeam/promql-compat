package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GreptimeTeam/promql-compat/internal/corpus"
	"github.com/GreptimeTeam/promql-compat/internal/datagen"
	"github.com/GreptimeTeam/promql-compat/internal/diff"
	"github.com/GreptimeTeam/promql-compat/internal/promref"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

// probe loads the demo dataset into both sides and prints each query's
// results, for investigating a divergence by hand. times is either an offset
// from the dataset start ("52m30s") or a range "40m..110m/10s".
func probe(ctx context.Context, tgt target.Target, runID string, queries []string, times, dataFile string) error {
	ref, err := target.NewPromref(promref.ProfileServer)
	if err != nil {
		return err
	}
	defer ref.S.Close()
	r := &diff.Runner{Ref: ref, Target: tgt}
	db := runID + "_probe"
	ds := datagen.Demo(1)
	base := datagen.DemoStart
	if dataFile != "" {
		raw, err := os.ReadFile(dataFile)
		if err != nil {
			return err
		}
		series, err := corpus.LoadSeries(ctx, string(raw))
		if err != nil {
			return err
		}
		ds = &datagen.Dataset{Series: series}
		base = time.Unix(0, 0).UTC()
	}
	if err := r.Load(ctx, db, ds); err != nil {
		return err
	}
	defer r.Drop(context.Background(), db)
	q, err := parseProbeTime(base, times)
	if err != nil {
		return err
	}
	for _, expr := range queries {
		ch := r.Check(ctx, db, expr, q)
		fmt.Printf("=== %s @ %s: %s\n", expr, q, ch.Status)
		for _, t := range []target.Target{ref, tgt} {
			res, err := diff.Run(ctx, t.Endpoint(db), expr, q)
			if err != nil {
				fmt.Printf("--- %s: error: %v\n", t.Name(), err)
				continue
			}
			s := res.Value.String()
			if len(s) > 3000 {
				s = s[:3000] + "..."
			}
			fmt.Printf("--- %s:\n%s\n", t.Name(), s)
		}
		fmt.Printf("--- check: %+v\n", ch)
	}
	return nil
}

func parseProbeTime(base time.Time, s string) (diff.Query, error) {
	if from, rest, ok := strings.Cut(s, ".."); ok {
		to, step, ok := strings.Cut(rest, "/")
		if !ok {
			return diff.Query{}, fmt.Errorf("range must be from..to/step")
		}
		f, err1 := time.ParseDuration(from)
		t, err2 := time.ParseDuration(to)
		st, err3 := time.ParseDuration(step)
		if err1 != nil || err2 != nil || err3 != nil {
			return diff.Query{}, fmt.Errorf("bad range %q", s)
		}
		return diff.Query{Start: base.Add(f), End: base.Add(t), Step: st}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return diff.Query{}, err
	}
	return diff.Query{Start: base.Add(d)}, nil
}
