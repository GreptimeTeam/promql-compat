// Package report aggregates cases into compatibility rates and renders them.
package report

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

type Report struct {
	Target   string        `json:"target"`
	Version  string        `json:"version"`
	Label    string        `json:"label,omitempty"`
	Config   Config        `json:"config"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Cases    []result.Case `json:"cases"`
}

// Config is everything besides the target that determines the cases and
// their results; two runs are comparable only if it matches.
type Config struct {
	Args []string `json:"args"`
	// Harness is the VCS revision of the harness build, with "+dirty" for
	// uncommitted changes; empty when built without VCS information.
	Harness string `json:"harness"`
	// Reference is the github.com/prometheus/prometheus module version.
	Reference string   `json:"reference"`
	Suites    []string `json:"suites"`
	Files     string   `json:"files,omitempty"`
	FuzzCount int      `json:"fuzz_count"`
	FuzzSeed  int64    `json:"fuzz_seed"`
}

// Diff lists the fields that differ between c and o.
func (c Config) Diff(o Config) []string {
	var out []string
	add := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, fmt.Sprintf("%s: %v -> %v", name, a, b))
		}
	}
	add("harness", c.Harness, o.Harness)
	add("reference", c.Reference, o.Reference)
	add("suites", c.Suites, o.Suites)
	add("files", c.Files, o.Files)
	add("fuzz_count", c.FuzzCount, o.FuzzCount)
	add("fuzz_seed", c.FuzzSeed, o.FuzzSeed)
	return out
}

// Rate is pass/total over in-scope cases.
type Rate struct {
	Total, Passed, Excluded int
}

func (r Rate) String() string {
	if r.Total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%% (%d/%d)", 100*float64(r.Passed)/float64(r.Total), r.Passed, r.Total)
}

func rateOf(cases []result.Case) Rate {
	var r Rate
	for i := range cases {
		c := &cases[i]
		if c.Excluded() {
			r.Excluded++
			continue
		}
		r.Total++
		if c.Status.Passed() {
			r.Passed++
		}
	}
	return r
}

// rateAll counts every case, including excluded ones.
func rateAll(cases []result.Case) Rate {
	var r Rate
	for _, c := range cases {
		r.Total++
		if c.Status.Passed() {
			r.Passed++
		}
	}
	return r
}

func (rep *Report) bySuite() (names []string, m map[string][]result.Case) {
	m = map[string][]result.Case{}
	for _, c := range rep.Cases {
		if _, ok := m[c.Suite]; !ok {
			names = append(names, c.Suite)
		}
		m[c.Suite] = append(m[c.Suite], c)
	}
	return names, m
}

// Summary is the short console form.
func (rep *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "target: %s %s\n", rep.Target, rep.Label)
	names, m := rep.bySuite()
	for _, n := range names {
		v := views(m[n])
		e := evidenceOf(m[n])
		fmt.Fprintf(&b, "  %-11s strict %s | primary %s | values-only %s | excluded %d | passed with data %d/%d\n",
			n, v[0], v[1], v[3], rateOf(m[n]).Excluded, e.data, e.passed)
	}
	return b.String()
}

func count(cases []result.Case, pred func(*result.Case) bool) int {
	n := 0
	for i := range cases {
		if pred(&cases[i]) {
			n++
		}
	}
	return n
}

// Markdown renders the full report.
func (rep *Report) Markdown() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# PromQL compatibility report\n\n")
	w("- Target: %s\n", rep.Target)
	if rep.Label != "" {
		w("- Label: %s\n", rep.Label)
	}
	if rep.Version != "" {
		w("- Build info: `%s`\n", strings.ReplaceAll(rep.Version, "\n", " "))
	}
	cfg := rep.Config
	w("- Reference: Prometheus engine, Go module github.com/prometheus/prometheus %s, embedded\n", orUnknown(cfg.Reference))
	w("- Harness revision: %s\n", orUnknown(cfg.Harness))
	w("- Suites: %s; corpus files: %s; fuzz: %d queries, seed %d\n", strings.Join(cfg.Suites, ","), orAll(cfg.Files), cfg.FuzzCount, cfg.FuzzSeed)
	if len(cfg.Args) > 0 {
		w("- Command: `%s`\n", oneLine(strings.Join(cfg.Args, " ")))
	}
	w("- Run: %s, %s\n\n", rep.Started.Format(time.RFC3339), rep.Finished.Sub(rep.Started).Round(time.Second))

	names, m := rep.bySuite()
	w("## Compatibility rate\n\n")
	w("A case passes when every query it issues returns the same result as Prometheus. ")
	w("Cases tagged native_histogram, start_timestamp, experimental, delayed_name_removal or nondeterministic are excluded from the rate and listed separately.\n\n")
	w("| Suite | Compatibility | Excluded | All cases |\n|---|---|---|---|\n")
	for _, n := range names {
		r := rateOf(m[n])
		w("| %s | %s | %d | %s |\n", n, r, r.Excluded, rateAll(m[n]))
	}
	w("\n")
	w("### Views\n\n")
	w("Same cases under narrower criteria, to separate single root causes from the rest. ")
	w("*Primary query*: only the query as written (corpus also runs `@`-modifier variants and a range-mode self-check). ")
	w("*Ignore negative timestamps*: checks failing only because the query time is before 1970 count as passed. ")
	w("*Values only*: checks whose result differs only in `__name__` count as passed.\n\n")
	w("| Suite | Strict | Primary query | Ignore negative timestamps | Values only | Primary + values only |\n|---|---|---|---|---|---|\n")
	for _, n := range names {
		v := views(m[n])
		w("| %s | %s | %s | %s | %s | %s |\n", n, v[0], v[1], v[2], v[3], v[4])
	}
	w("\n")
	w("### Evidence of passed cases\n\n")
	w("Two empty results or two errors agree without exercising the evaluation. ")
	w("*Data*: at least one query returned a non-empty result. *Empty only*: every passed query returned an empty result or an error on both sides. *Errors only*: every query failed on both sides.\n\n")
	w("| Suite | Passed (in scope) | Data | Empty only | Errors only |\n|---|---|---|---|---|\n")
	for _, n := range names {
		e := evidenceOf(m[n])
		w("| %s | %d | %d | %d | %d |\n", n, e.passed, e.data, e.empty, e.errs)
	}
	w("\n")
	coverage(w, names, m)

	for _, n := range names {
		cases := m[n]
		w("## Suite `%s`\n\n", n)
		w("### Outcomes\n\n| Status | In scope | Excluded |\n|---|---|---|\n")
		for _, s := range []result.Status{result.Pass, result.PassUnsorted, result.PassErrorMessage, result.FailValue, result.FailNoError, result.FailError, result.FailIngest, result.FailTransport} {
			in := count(cases, func(c *result.Case) bool { return c.Status == s && !c.Excluded() })
			ex := count(cases, func(c *result.Case) bool { return c.Status == s && c.Excluded() })
			if in+ex > 0 {
				w("| %s | %d | %d |\n", s, in, ex)
			}
		}
		ann := count(cases, func(c *result.Case) bool {
			for _, ch := range c.Checks {
				if ch.Annotation != "" {
					return true
				}
			}
			return false
		})
		w("\nCases with warning/info annotation mismatches (not counted): %d\n\n", ann)

		w("### Failure causes (in scope)\n\n")
		w("A case can fail for several causes. *Sole* counts cases where every failed check has this cause, i.e. the cases fixing this class alone would recover.\n\n")
		w("| Cause | Cases involved | Sole cause |\n|---|---|---|\n")
		involved, sole := map[string]int{}, map[string]int{}
		for i := range cases {
			c := &cases[i]
			if c.Excluded() || c.Status.Passed() {
				continue
			}
			cs := c.Causes()
			for _, x := range cs {
				involved[x]++
			}
			if len(cs) == 1 {
				sole[cs[0]]++
			}
		}
		var causes []string
		for x := range involved {
			causes = append(causes, x)
		}
		sort.Slice(causes, func(i, j int) bool { return involved[causes[i]] > involved[causes[j]] })
		for _, x := range causes {
			w("| %s | %d | %d |\n", x, involved[x], sole[x])
		}
		w("\n")
		w("### Top failure signatures (cause = other)\n\n| Checks | Signature | Example |\n|---|---|---|\n")
		for _, sg := range signatures(cases, 40) {
			w("| %d | %s | `%s` @ %s |\n", sg.n, oneLine(sg.sig), oneLine(truncate(sg.query, 120)), sg.time)
		}
		w("\n")

		groups := map[string][]result.Case{}
		var gnames []string
		for _, c := range cases {
			if _, ok := groups[c.Group]; !ok {
				gnames = append(gnames, c.Group)
			}
			groups[c.Group] = append(groups[c.Group], c)
		}
		sort.Strings(gnames)
		w("### By group\n\n| Group | Compatibility | Excluded |\n|---|---|---|\n")
		for _, g := range gnames {
			r := rateOf(groups[g])
			w("| %s | %s | %d |\n", g, r, r.Excluded)
		}
		w("\n### Failures (in scope)\n\n")
		for _, g := range gnames {
			var fails []result.Case
			for _, c := range groups[g] {
				if !c.Excluded() && !c.Status.Passed() {
					fails = append(fails, c)
				}
			}
			if len(fails) == 0 {
				continue
			}
			w("#### %s (%d)\n\n", g, len(fails))
			for _, c := range fails {
				w("- `%s` `%s` **%s**\n", c.ID, oneLine(c.Expr), c.Status)
				if c.Minimized != "" && c.Minimized != c.Expr {
					w("  - minimized: `%s`\n", oneLine(c.Minimized))
				}
				for _, ch := range c.Checks {
					if !ch.Status.Passed() {
						w("  - %s `%s` at %s: %s\n", ch.Kind, oneLine(ch.Query), ch.Time, oneLine(truncate(ch.Detail, 400)))
						break
					}
				}
			}
			w("\n")
		}
		w("### Excluded cases by tag\n\n| Tag | Cases | Of which pass |\n|---|---|---|\n")
		tagCount := map[string][2]int{}
		for _, c := range cases {
			for _, t := range c.Tags {
				v := tagCount[t]
				v[0]++
				if c.Status.Passed() {
					v[1]++
				}
				tagCount[t] = v
			}
		}
		var tags []string
		for t := range tagCount {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		for _, t := range tags {
			w("| %s | %d | %d |\n", t, tagCount[t][0], tagCount[t][1])
		}
		w("\n")
	}
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orAll(s string) string {
	if s == "" {
		return "all"
	}
	return s
}

type evidenceCounts struct{ passed, data, empty, errs int }

func evidenceOf(cases []result.Case) evidenceCounts {
	var e evidenceCounts
	for i := range cases {
		c := &cases[i]
		if c.Excluded() || !c.Status.Passed() {
			continue
		}
		e.passed++
		switch c.Evidence() {
		case result.EvidenceData:
			e.data++
		case result.EvidenceEmpty:
			e.empty++
		case result.EvidenceError:
			e.errs++
		}
	}
	return e
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "'")), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var (
	reLabels = regexp.MustCompile(`\{[^{}]*\}`)
	reNums   = regexp.MustCompile(`[-+]?(\d+(\.\d+)?(e[-+]?\d+)?|Inf|NaN)`)
)

type signature struct {
	sig, query, time string
	n                int
}

// signatures groups failed checks of cause "other" by their detail with
// label sets and numbers masked.
func signatures(cases []result.Case, limit int) []signature {
	m := map[string]*signature{}
	for _, c := range cases {
		if c.Excluded() || c.Status.Passed() {
			continue
		}
		for _, ch := range c.Checks {
			if ch.Status.Passed() || ch.Cause != result.CauseOther {
				continue
			}
			d := ch.Detail
			if i := strings.Index(d, " | "); i > 0 {
				d = d[:i]
			}
			d = reNums.ReplaceAllString(reLabels.ReplaceAllString(d, "{…}"), "N")
			d = string(ch.Status) + ": " + truncate(d, 140)
			sg, ok := m[d]
			if !ok {
				sg = &signature{sig: d, query: ch.Query, time: ch.Time}
				m[d] = sg
			}
			sg.n++
		}
	}
	out := make([]signature, 0, len(m))
	for _, sg := range m {
		out = append(out, *sg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].sig < out[j].sig
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// views computes the rate under progressively narrower pass criteria.
func views(cases []result.Case) [5]Rate {
	ok := func(ch result.Check, ignore ...string) bool {
		if ch.Status.Passed() {
			return true
		}
		for _, c := range ignore {
			if ch.Cause == c {
				return true
			}
		}
		return false
	}
	all := func(c *result.Case, primaryOnly bool, ignore ...string) bool {
		for _, ch := range c.Checks {
			if primaryOnly && (ch.Kind == "instant_at" || ch.Kind == "range_mode") {
				continue
			}
			if !ok(ch, ignore...) {
				return false
			}
		}
		return true
	}
	preds := [5]func(c *result.Case) bool{
		func(c *result.Case) bool { return all(c, false) },
		func(c *result.Case) bool { return all(c, true) },
		func(c *result.Case) bool { return all(c, false, result.CauseNegativeTS) },
		func(c *result.Case) bool { return all(c, false, result.CauseMetricName) },
		func(c *result.Case) bool { return all(c, true, result.CauseMetricName) },
	}
	var out [5]Rate
	for i, p := range preds {
		for j := range cases {
			c := &cases[j]
			if c.Excluded() {
				continue
			}
			out[i].Total++
			if len(c.Checks) > 0 && p(c) {
				out[i].Passed++
			}
		}
	}
	return out
}
