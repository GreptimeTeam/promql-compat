package report

import (
	"fmt"
	"sort"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

var stdParser = parser.NewParser(parser.Options{})

type coverageCell struct{ total, passed, data int }

// coverage renders, per PromQL construct and suite, how many in-scope cases
// use it, how many pass and how many of those passed with data. Every
// non-experimental function is listed, so constructs without any case show up.
func coverage(w func(string, ...any), names []string, m map[string][]result.Case) {
	cells := map[string]map[string]*coverageCell{}
	add := func(construct, suite string, c *result.Case) {
		if cells[construct] == nil {
			cells[construct] = map[string]*coverageCell{}
		}
		cell := cells[construct][suite]
		if cell == nil {
			cell = &coverageCell{}
			cells[construct][suite] = cell
		}
		cell.total++
		if c.Status.Passed() {
			cell.passed++
			if c.Evidence() == result.EvidenceData {
				cell.data++
			}
		}
	}
	for name, f := range parser.Functions {
		if !f.Experimental {
			cells["function "+name] = map[string]*coverageCell{}
		}
	}
	for _, n := range names {
		for i := range m[n] {
			c := &m[n][i]
			if c.Excluded() {
				continue
			}
			e, err := stdParser.ParseExpr(c.Expr)
			if err != nil {
				continue
			}
			for construct := range constructs(e) {
				add(construct, n, c)
			}
		}
	}
	rows := make([]string, 0, len(cells))
	for k := range cells {
		rows = append(rows, k)
	}
	sort.Strings(rows)

	w("## Coverage\n\n")
	w("In-scope cases per construct, as *passed/total (data)*, where *data* counts passed cases with at least one non-empty result.\n\n")
	w("| Construct |")
	for _, n := range names {
		w(" %s |", n)
	}
	w("\n|---|")
	for range names {
		w("---|")
	}
	w("\n")
	for _, r := range rows {
		w("| %s |", r)
		for _, n := range names {
			if cell := cells[r][n]; cell != nil {
				w(" %d/%d (%d) |", cell.passed, cell.total, cell.data)
			} else {
				w(" 0 |")
			}
		}
		w("\n")
	}
	w("\n")
}

// constructs returns the functions, aggregations and binary operators in e.
func constructs(e parser.Expr) map[string]bool {
	out := map[string]bool{}
	parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
		switch x := n.(type) {
		case *parser.Call:
			out["function "+x.Func.Name] = true
		case *parser.AggregateExpr:
			out["aggregation "+x.Op.String()] = true
		case *parser.BinaryExpr:
			out[fmt.Sprintf("operator %s", x.Op)] = true
		}
		return nil
	})
	return out
}
