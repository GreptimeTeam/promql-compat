package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/GreptimeTeam/promql-compat/internal/report"
)

func loadReport(path string) (*report.Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// compareRuns prints, per suite, the in-scope cases that flipped between two
// runs and returns the number of regressions.
func compareRuns(basePath, newPath string) (int, error) {
	base, err := loadReport(basePath)
	if err != nil {
		return 0, err
	}
	cur, err := loadReport(newPath)
	if err != nil {
		return 0, err
	}
	cmp := report.Compare(base, cur)
	fmt.Printf("%s -> %s\n", base.Label, cur.Label)
	for _, d := range base.Config.Diff(cur.Config) {
		fmt.Println("  CONFIG DIFFERS", d)
	}
	for _, c := range cmp.Suites {
		fmt.Printf("  %-11s fixed %d, regressed %d, only in base %d, only in new %d, scope changed %d\n",
			c.Suite, c.Fixed, c.Regressed, c.OnlyBase, c.OnlyNew, c.ScopeChanged)
	}
	for _, c := range cmp.Regressed {
		detail := ""
		if ch := report.FirstFailure(c); ch != nil {
			detail = ch.Detail
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		fmt.Printf("  REGRESSION %s %s %q: %s\n", c.ID, c.Status, c.Expr, detail)
	}
	return len(cmp.Regressed), nil
}

// compareHTML writes a comparison page of two or more runs, oldest first.
func compareHTML(out string, paths []string) error {
	if len(paths) < 2 {
		return fmt.Errorf("-compare-html needs at least two results.json files, got %d", len(paths))
	}
	reps := make([]*report.Report, len(paths))
	for i, p := range paths {
		r, err := loadReport(p)
		if err != nil {
			return err
		}
		reps[i] = r
	}
	page, err := report.CompareHTML(reps)
	if err != nil {
		return err
	}
	return os.WriteFile(out, []byte(page), 0o644)
}
