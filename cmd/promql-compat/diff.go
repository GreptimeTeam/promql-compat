package main

import (
	"context"
	"fmt"

	"github.com/GreptimeTeam/promql-compat/internal/compliance"
	"github.com/GreptimeTeam/promql-compat/internal/datagen"
	"github.com/GreptimeTeam/promql-compat/internal/diff"
	"github.com/GreptimeTeam/promql-compat/internal/fuzz"
	"github.com/GreptimeTeam/promql-compat/internal/promref"
	"github.com/GreptimeTeam/promql-compat/internal/result"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

// diffEnv holds the demo dataset, loaded once into two server-profile
// references and the target. The differential suites share it: a target
// without dataset isolation would otherwise store every sample twice.
type diffEnv struct {
	r         *diff.Runner
	ref, ref2 *target.Promref
	db        string
	ds        *datagen.Dataset
	keep      bool
}

func newDiffEnv(ctx context.Context, tgt target.Target, runID string, keep bool) (*diffEnv, error) {
	ref, err := target.NewPromref(promref.ProfileServer)
	if err != nil {
		return nil, err
	}
	ref2, err := target.NewPromref(promref.ProfileServer)
	if err != nil {
		ref.S.Close()
		return nil, err
	}
	e := &diffEnv{
		r:   &diff.Runner{Ref: ref, Ref2: ref2, Target: tgt},
		ref: ref, ref2: ref2,
		db:   runID + "_demo",
		ds:   datagen.Demo(1),
		keep: keep,
	}
	if err := e.r.Load(ctx, e.db, e.ds); err != nil {
		e.close()
		return nil, err
	}
	return e, nil
}

func (e *diffEnv) run(ctx context.Context, suite string, n int, seed int64, workers int, minimize bool) ([]result.Case, error) {
	switch suite {
	case "compliance":
		return compliance.Run(ctx, e.r, e.db, e.ds, workers)
	case "fuzz":
		return fuzz.Run(ctx, e.r, e.db, n, seed, workers, minimize)
	default:
		return nil, fmt.Errorf("suite %s is not differential", suite)
	}
}

func (e *diffEnv) close() {
	if !e.keep {
		e.r.Drop(context.Background(), e.db)
	}
	e.ref.S.Close()
	e.ref2.S.Close()
}
