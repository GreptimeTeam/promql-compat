// Package diff runs the same queries against the reference and the target on
// the same dataset and compares the answers.
package diff

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/GreptimeTeam/promql-compat/internal/client"
	"github.com/GreptimeTeam/promql-compat/internal/compare"
	"github.com/GreptimeTeam/promql-compat/internal/datagen"
	"github.com/GreptimeTeam/promql-compat/internal/result"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

// Query is one evaluation: instant when Step == 0, otherwise range.
type Query struct {
	Start, End time.Time
	Step       time.Duration
}

func (q Query) String() string {
	if q.Step == 0 {
		return client.FormatTime(q.Start.UnixMilli())
	}
	return fmt.Sprintf("%s..%s/%s", client.FormatTime(q.Start.UnixMilli()), client.FormatTime(q.End.UnixMilli()), q.Step)
}

type Runner struct {
	Ref, Target target.Target
	// Ref2 is a second reference instance with the same data. A query whose
	// two reference answers differ has no unique answer to compare against.
	Ref2    target.Target
	Timeout time.Duration
}

// Load creates db on every side, writes the dataset and waits until it is
// queryable.
func (r *Runner) Load(ctx context.Context, db string, ds *datagen.Dataset) error {
	for _, t := range r.targets() {
		if err := t.CreateDB(ctx, db); err != nil {
			return fmt.Errorf("%s: create %s: %w", t.Name(), db, err)
		}
		if err := t.Endpoint(db).Write(ctx, ds.Series); err != nil {
			return fmt.Errorf("%s: write %s: %w", t.Name(), db, err)
		}
		if err := t.Ready(ctx, db); err != nil {
			return fmt.Errorf("%s: ready %s: %w", t.Name(), db, err)
		}
	}
	return nil
}

func (r *Runner) targets() []target.Target {
	ts := []target.Target{r.Ref, r.Target}
	if r.Ref2 != nil {
		ts = append(ts, r.Ref2)
	}
	return ts
}

func (r *Runner) Drop(ctx context.Context, db string) {
	for _, t := range r.targets() {
		_ = t.DropDB(ctx, db)
	}
}

// Run evaluates expr as an instant or range query.
func Run(ctx context.Context, ep *client.Endpoint, expr string, q Query) (*client.Result, error) {
	if q.Step == 0 {
		return ep.Instant(ctx, expr, q.Start)
	}
	return ep.Range(ctx, expr, q.Start, q.End, q.Step)
}

// Check evaluates expr on both sides.
func (r *Runner) Check(ctx context.Context, db, expr string, q Query) result.Check {
	kind := "range"
	if q.Step == 0 {
		kind = "instant"
	}
	ch := result.Check{Kind: kind, Query: expr, Time: q.String()}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ref, rerr := Run(cctx, r.Ref.Endpoint(db), expr, q)
	got, gerr := Run(cctx, r.Target.Endpoint(db), expr, q)
	var qe *client.QueryError
	if rerr != nil && !errors.As(rerr, &qe) {
		ch.Status, ch.Detail = result.FailTransport, "reference: "+rerr.Error()
		return ch
	}
	if gerr != nil && !errors.As(gerr, &qe) {
		ch.Status, ch.Detail = result.FailTransport, gerr.Error()
		return ch
	}
	switch {
	case rerr != nil && gerr != nil:
		ch.Status, ch.Evidence = result.Pass, result.EvidenceError
		if rerr.Error() != gerr.Error() {
			ch.Status = result.PassErrorMessage
			ch.Detail = fmt.Sprintf("both failed; reference: %s; target: %s", rerr, gerr)
		}
	case rerr != nil:
		ch.Status = result.FailNoError
		ch.Detail = fmt.Sprintf("reference failed with %q, target returned %s", rerr, truncate(got.Value.String(), 300))
	case gerr != nil:
		ch.Status, ch.Detail = result.FailError, gerr.Error()
	default:
		ordered := false
		if e, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(expr); err == nil && q.Step == 0 {
			ordered = compare.Ordered(e)
		}
		if r.Ref2 != nil {
			// Tie-breaking and float summation order vary between runs, so
			// order-sensitive queries get more reference samples.
			probes := 1
			if reOrderSensitive.MatchString(expr) {
				probes = 4
			}
			for i := 0; i < probes && !ch.Nondeterministic; i++ {
				ep := r.Ref2.Endpoint(db)
				if i%2 == 1 {
					ep = r.Ref.Endpoint(db)
				}
				ref2, err := Run(cctx, ep, expr, q)
				if err != nil {
					// No second answer, so no evidence of nondeterminism.
					ch.Status, ch.Detail = result.FailTransport, "reference recheck: "+err.Error()
					return ch
				}
				if st, _ := compare.Values(ref.Value, ref2.Value, ordered); !st.Passed() {
					ch.Nondeterministic = true
				}
			}
		}
		ch.Status, ch.Detail = compare.Values(ref.Value, got.Value, ordered)
		if ch.Status.Passed() {
			ch.Evidence = result.EvidenceOf(ref.Empty())
		}
		if ch.Status == result.FailValue {
			if st, _ := compare.Values(compare.DropName(ref.Value), compare.DropName(got.Value), ordered); st.Passed() {
				ch.Cause = result.CauseMetricName
			}
		}
	}
	return ch
}

var reOrderSensitive = regexp.MustCompile(`topk|bottomk|limitk|limit_ratio|count_values|stddev|stdvar`)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
