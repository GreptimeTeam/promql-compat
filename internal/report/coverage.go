package report

import (
	"fmt"
	"sort"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

var stdParser = parser.NewParser(parser.Options{})

type coverageCell struct{ Total, Passed, Data int }

type coverageRow struct {
	Construct string
	// Cells is indexed like the suite names; nil when no case uses the construct.
	Cells []*coverageCell
}

// coverageTable counts, per PromQL construct and suite, how many in-scope cases
// use it, how many pass and how many of those passed with data. Every
// non-experimental function is listed, so constructs without any case show up.
func coverageTable(names []string, m map[string][]result.Case) []coverageRow {
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
		cell.Total++
		if c.Status.Passed() {
			cell.Passed++
			if c.Evidence() == result.EvidenceData {
				cell.Data++
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
	keys := make([]string, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]coverageRow, 0, len(keys))
	for _, k := range keys {
		row := coverageRow{Construct: k, Cells: make([]*coverageCell, len(names))}
		for i, n := range names {
			row.Cells[i] = cells[k][n]
		}
		rows = append(rows, row)
	}
	return rows
}

func coverage(w func(string, ...any), names []string, m map[string][]result.Case) {
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
	for _, r := range coverageTable(names, m) {
		w("| %s |", r.Construct)
		for _, cell := range r.Cells {
			if cell != nil {
				w(" %d/%d (%d) |", cell.Passed, cell.Total, cell.Data)
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
