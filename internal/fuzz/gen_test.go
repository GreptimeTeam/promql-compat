package fuzz

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

// Every generated query must parse with the default Prometheus parser;
// otherwise the fuzz suite measures generator bugs.
func TestGeneratedQueriesParse(t *testing.T) {
	g := NewGen(42)
	p := parser.NewParser(parser.Options{})
	for i := 0; i < 20000; i++ {
		q := g.Query()
		if _, err := p.ParseExpr(q); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
}

func TestGeneratorDeterministic(t *testing.T) {
	a, b := NewGen(7), NewGen(7)
	for i := 0; i < 1000; i++ {
		if qa, qb := a.Query(), b.Query(); qa != qb {
			t.Fatalf("iteration %d: %q != %q", i, qa, qb)
		}
	}
}
