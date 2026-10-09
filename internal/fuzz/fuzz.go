package fuzz

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/GreptimeTeam/promql-compat/internal/datagen"
	"github.com/GreptimeTeam/promql-compat/internal/diff"
	"github.com/GreptimeTeam/promql-compat/internal/par"
	"github.com/GreptimeTeam/promql-compat/internal/result"
)

var stdParser = parser.NewParser(parser.Options{})

// Evaluations used per generated query: a range over the outage/restart
// stretch and two instants.
func Evaluations() []diff.Query {
	s := datagen.DemoStart
	return []diff.Query{
		{Start: s.Add(40 * time.Minute), End: s.Add(110 * time.Minute), Step: 10 * time.Second},
		{Start: s.Add(52*time.Minute + 30*time.Second)},
		{Start: s.Add(70*time.Minute + 7*time.Second)},
	}
}

// Run generates n queries from seed, runs them on db (the demo dataset) and
// shrinks the failures.
func Run(ctx context.Context, r *diff.Runner, db string, n int, seed int64, workers int, minimize bool) ([]result.Case, error) {
	g := NewGen(seed)
	seen := map[string]bool{}
	var queries []string
	for i := 0; len(queries) < n && i < 50*n; i++ {
		if q := g.Query(); !seen[q] {
			seen[q] = true
			queries = append(queries, q)
		}
	}
	cases := make([]result.Case, len(queries))
	par.Do(len(queries), workers, func(i int) {
		q := queries[i]
		c := result.Case{Suite: "fuzz", ID: fmt.Sprintf("fuzz:%d:%d", seed, i+1), Group: shape(q), Expr: q}
		if ctx.Err() != nil {
			return
		}
		c.Checks = evalAll(ctx, r, db, q)
		c.Finish()
		if minimize && !c.Status.Passed() {
			c.Minimized = shrink(ctx, r, db, q, c.Status, c.Causes())
		}
		cases[i] = c
	})
	return cases, ctx.Err()
}

func evalAll(ctx context.Context, r *diff.Runner, db, q string) []result.Check {
	var out []result.Check
	for _, ev := range Evaluations() {
		out = append(out, r.Check(ctx, db, q, ev))
	}
	return out
}

// shape names the top-level construct, used to group fuzz cases.
func shape(q string) string {
	e, err := stdParser.ParseExpr(q)
	if err != nil {
		return "unparsable"
	}
	for {
		switch n := e.(type) {
		case *parser.ParenExpr:
			e = n.Expr
			continue
		case *parser.Call:
			return "call " + n.Func.Name
		case *parser.AggregateExpr:
			return "aggregate " + n.Op.String()
		case *parser.BinaryExpr:
			l, r := n.LHS.Type(), n.RHS.Type()
			return fmt.Sprintf("binary %s %s %s", l, n.Op, r)
		case *parser.UnaryExpr:
			return "unary"
		case *parser.SubqueryExpr:
			return "subquery"
		case *parser.VectorSelector:
			return "selector"
		case *parser.NumberLiteral:
			return "number"
		default:
			return fmt.Sprintf("%T", e)
		}
	}
}

// shrink greedily replaces the query by smaller variants that still fail
// with the same status and causes, and returns the smallest found.
func shrink(ctx context.Context, r *diff.Runner, db, q string, status result.Status, causes []string) string {
	same := func(cand string) bool {
		c := result.Case{Checks: evalAll(ctx, r, db, cand)}
		c.Finish()
		return c.Status == status && equalSets(c.Causes(), causes)
	}
	best := q
	budget := 40
	for budget > 0 {
		improved := false
		for _, cand := range variants(best) {
			if budget--; budget < 0 {
				break
			}
			if same(cand) {
				best = cand
				improved = true
				break
			}
		}
		if !improved {
			break
		}
	}
	return best
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// variants returns strictly shorter candidate reductions of q, shortest first.
func variants(q string) []string {
	root, err := stdParser.ParseExpr(q)
	if err != nil {
		return nil
	}
	var out []string
	add := func(s string) {
		if len(s) < len(q) {
			if _, err := stdParser.ParseExpr(s); err == nil {
				out = append(out, s)
			}
		}
	}
	// Sub-expressions of an instant type replace the whole query.
	parser.Inspect(root, func(n parser.Node, _ []parser.Node) error {
		if e, ok := n.(parser.Expr); ok && e != root {
			if t := e.Type(); t == parser.ValueTypeVector || t == parser.ValueTypeScalar {
				add(e.String())
			}
		}
		return nil
	})
	// Local edits, applied on a fresh parse each time.
	count := 0
	parser.Inspect(root, func(parser.Node, []parser.Node) error { count++; return nil })
	for i := 0; i < count; i++ {
		for k := 0; k < 8; k++ {
			if s, ok := edit(q, i, k); ok {
				add(s)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return dedup(out)
}

// edit applies edit kind k to the i-th node of a fresh parse of q.
func edit(q string, i, k int) (string, bool) {
	root, err := stdParser.ParseExpr(q)
	if err != nil {
		return "", false
	}
	idx := 0
	changed := false
	parser.Inspect(root, func(n parser.Node, _ []parser.Node) error {
		defer func() { idx++ }()
		if idx != i {
			return nil
		}
		switch x := n.(type) {
		case *parser.VectorSelector:
			var others []int
			for j, m := range x.LabelMatchers {
				if m.Name != "__name__" {
					others = append(others, j)
				}
			}
			switch {
			case k < len(others):
				x.LabelMatchers = append(x.LabelMatchers[:others[k]:others[k]], x.LabelMatchers[others[k]+1:]...)
				changed = true
			case k == 7 && (x.OriginalOffset != 0 || x.Timestamp != nil || x.StartOrEnd != 0):
				x.OriginalOffset, x.Timestamp, x.StartOrEnd = 0, nil, 0
				changed = true
			}
		case *parser.AggregateExpr:
			if k == 0 && (len(x.Grouping) > 0 || x.Without) {
				x.Grouping, x.Without = nil, false
				changed = true
			}
		case *parser.BinaryExpr:
			if k == 0 && x.VectorMatching != nil && (len(x.VectorMatching.MatchingLabels) > 0 || x.VectorMatching.On) &&
				x.VectorMatching.Card == parser.CardOneToOne {
				x.VectorMatching.MatchingLabels, x.VectorMatching.On = nil, false
				changed = true
			}
		case *parser.MatrixSelector:
			if k == 0 && x.Range > time.Minute {
				x.Range = time.Minute
				changed = true
			}
		}
		return nil
	})
	if !changed {
		return "", false
	}
	return root.String(), true
}

func dedup(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
