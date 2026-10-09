package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/GreptimeTeam/promql-compat/internal/report"
	"github.com/GreptimeTeam/promql-compat/internal/result"
)

// compareRuns prints, per suite, the in-scope cases that flipped between two
// runs, keyed by case ID and expression, and returns the number of
// regressions. Cases present in only one run, or in scope in only one, are
// counted but not compared.
func compareRuns(basePath, newPath string) (int, error) {
	load := func(p string) (*report.Report, error) {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r report.Report
		return &r, json.Unmarshal(raw, &r)
	}
	base, err := load(basePath)
	if err != nil {
		return 0, err
	}
	cur, err := load(newPath)
	if err != nil {
		return 0, err
	}
	key := func(c *result.Case) string { return c.ID + "\x00" + c.Expr }
	baseCases := map[string]*result.Case{}
	for i := range base.Cases {
		baseCases[key(&base.Cases[i])] = &base.Cases[i]
	}
	type counts struct{ fixed, regressed, onlyBase, onlyNew, scopeChanged int }
	per := map[string]*counts{}
	get := func(suite string) *counts {
		if per[suite] == nil {
			per[suite] = &counts{}
		}
		return per[suite]
	}
	var regressions []string
	matched := map[string]bool{}
	for i := range cur.Cases {
		c := &cur.Cases[i]
		cnt := get(c.Suite)
		b, ok := baseCases[key(c)]
		if !ok {
			cnt.onlyNew++
			continue
		}
		matched[key(c)] = true
		if c.Excluded() != b.Excluded() {
			cnt.scopeChanged++
			continue
		}
		if c.Excluded() {
			continue
		}
		switch {
		case c.Status.Passed() && !b.Status.Passed():
			cnt.fixed++
		case !c.Status.Passed() && b.Status.Passed():
			cnt.regressed++
			detail := ""
			for _, ch := range c.Checks {
				if !ch.Status.Passed() {
					detail = ch.Detail
					break
				}
			}
			if len(detail) > 200 {
				detail = detail[:200]
			}
			regressions = append(regressions, fmt.Sprintf("%s %s %q: %s", c.ID, c.Status, c.Expr, detail))
		}
	}
	for i := range base.Cases {
		if !matched[key(&base.Cases[i])] {
			get(base.Cases[i].Suite).onlyBase++
		}
	}
	var suites []string
	for s := range per {
		suites = append(suites, s)
	}
	sort.Strings(suites)
	fmt.Printf("%s -> %s\n", base.Label, cur.Label)
	for _, d := range base.Config.Diff(cur.Config) {
		fmt.Println("  CONFIG DIFFERS", d)
	}
	total := 0
	for _, s := range suites {
		c := per[s]
		total += c.regressed
		fmt.Printf("  %-11s fixed %d, regressed %d, only in base %d, only in new %d, scope changed %d\n",
			s, c.fixed, c.regressed, c.onlyBase, c.onlyNew, c.scopeChanged)
	}
	for _, r := range regressions {
		fmt.Println("  REGRESSION", r)
	}
	return total, nil
}
