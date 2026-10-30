package report

import (
	"sort"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

// SuiteChanges counts how the in-scope cases of one suite changed between two
// runs. Cases present in only one run, or in scope in only one, are counted
// but not compared.
type SuiteChanges struct {
	Suite                                             string
	Fixed, Regressed, OnlyBase, OnlyNew, ScopeChanged int
}

// Comparison is the case-level difference between a base run and a newer one.
type Comparison struct {
	Suites []SuiteChanges // sorted by suite name
	// Fixed and Regressed hold the newer run's cases, in its order.
	Fixed, Regressed []*result.Case
}

// Compare matches cases by ID and expression.
func Compare(base, cur *Report) Comparison {
	key := func(c *result.Case) string { return c.ID + "\x00" + c.Expr }
	baseCases := map[string]*result.Case{}
	for i := range base.Cases {
		baseCases[key(&base.Cases[i])] = &base.Cases[i]
	}
	per := map[string]*SuiteChanges{}
	get := func(suite string) *SuiteChanges {
		if per[suite] == nil {
			per[suite] = &SuiteChanges{Suite: suite}
		}
		return per[suite]
	}
	var cmp Comparison
	matched := map[string]bool{}
	for i := range cur.Cases {
		c := &cur.Cases[i]
		cnt := get(c.Suite)
		b, ok := baseCases[key(c)]
		if !ok {
			cnt.OnlyNew++
			continue
		}
		matched[key(c)] = true
		if c.Excluded() != b.Excluded() {
			cnt.ScopeChanged++
			continue
		}
		if c.Excluded() {
			continue
		}
		switch {
		case c.Status.Passed() && !b.Status.Passed():
			cnt.Fixed++
			cmp.Fixed = append(cmp.Fixed, c)
		case !c.Status.Passed() && b.Status.Passed():
			cnt.Regressed++
			cmp.Regressed = append(cmp.Regressed, c)
		}
	}
	for i := range base.Cases {
		if !matched[key(&base.Cases[i])] {
			get(base.Cases[i].Suite).OnlyBase++
		}
	}
	for _, s := range per {
		cmp.Suites = append(cmp.Suites, *s)
	}
	sort.Slice(cmp.Suites, func(i, j int) bool { return cmp.Suites[i].Suite < cmp.Suites[j].Suite })
	return cmp
}

// FirstFailure returns the first failed check of c.
func FirstFailure(c *result.Case) *result.Check {
	for i := range c.Checks {
		if !c.Checks[i].Status.Passed() {
			return &c.Checks[i]
		}
	}
	return nil
}
