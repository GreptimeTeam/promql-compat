package report

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/GreptimeTeam/promql-compat/internal/result"
)

type runInfo struct {
	Name, Target, Build, Harness, Started string
}

func runInfoOf(rep *Report) runInfo {
	r := runInfo{
		Target:  rep.Target,
		Build:   buildLine(rep.Version),
		Harness: shortRevision(rep.Config.Harness),
		Started: rep.Started.Format("2006-01-02 15:04"),
	}
	switch {
	case rep.Label != "":
		r.Name = rep.Label
	case r.Build != "":
		r.Name = r.Build
	default:
		r.Name = rep.Target
	}
	return r
}

type suiteTile struct {
	Suite                    string
	Strict, ValuesOnly       Rate
	StrictDelta, ValuesDelta float64
	HasDelta                 bool
	FirstName, LastName      string
	FirstStrict              Rate
}

type chartPoint struct {
	X, Y   float64
	Run    string
	Series string
	Rate   Rate
}

type chartLabel struct {
	X, Y   float64
	Text   string
	Anchor string
}

type trendChart struct {
	Suite                    string
	Width, Height            float64
	Grid                     []chartLabel // Y is the line position, Text the tick
	XLabels                  []chartLabel
	StrictPath, ValuesPath   string
	Strict, Values           []chartPoint
	StrictLabel, ValuesLabel *chartLabel
	PlotLeft, PlotRight      float64
}

type caseRow struct {
	Suite, Group, Expr, Minimized string
	Status                        result.Status
	Query, Time, Detail           string
}

type groupCount struct {
	Suite, Group string
	N            int
}

type pairData struct {
	From, To   string
	ConfigDiff []string
	Suites     []SuiteChanges
	Regressed  []caseRow
	Fixed      []caseRow
	FixedBy    []groupCount
}

type construct struct {
	Name                  string
	FirstPass, FirstTotal int
	LastPass, LastTotal   int
	Delta                 float64
}

type compareData struct {
	Title   string
	Runs    []runInfo
	Tiles   []suiteTile
	Charts  []trendChart
	Pairs   []pairData
	Movers  []construct
	First   string
	Last    string
	Summary [][]string // per run: strict and ignoring-__name__ rate of each suite
	Suites  []string
}

// CompareHTML renders how compatibility changes over runs, given oldest
// first: rates per run, cases fixed and regressed between consecutive runs,
// and the constructs whose pass rate moved most from the first run to the last.
func CompareHTML(reps []*Report) (string, error) {
	d := compareData{}
	var suites []string
	seen := map[string]bool{}
	byRun := make([]map[string][]result.Case, len(reps))
	for i, rep := range reps {
		d.Runs = append(d.Runs, runInfoOf(rep))
		names, m := rep.bySuite()
		byRun[i] = m
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				suites = append(suites, n)
			}
		}
	}
	d.Suites = suites
	d.First, d.Last = d.Runs[0].Name, d.Runs[len(d.Runs)-1].Name
	names := make([]string, len(d.Runs))
	for i, r := range d.Runs {
		names[i] = r.Name
	}
	if len(names) <= 3 {
		d.Title = strings.Join(names, " → ")
	} else {
		d.Title = fmt.Sprintf("%s → %s (%d runs)", d.First, d.Last, len(names))
	}

	// rates[suite][run]; Total 0 when the run lacks the suite.
	rates := map[string][][2]Rate{}
	for _, s := range suites {
		rs := make([][2]Rate, len(reps))
		for i := range reps {
			if cases := byRun[i][s]; len(cases) > 0 {
				v := views(cases)
				rs[i] = [2]Rate{v[0], v[3]}
			}
		}
		rates[s] = rs
	}
	for _, s := range suites {
		d.Tiles = append(d.Tiles, tileOf(s, rates[s], names))
		d.Charts = append(d.Charts, chartOf(s, rates[s], names))
	}
	for i := range d.Runs {
		row := []string{}
		for _, s := range suites {
			r := rates[s][i]
			row = append(row, r[0].String(), r[1].String())
		}
		d.Summary = append(d.Summary, row)
	}
	for i := 1; i < len(reps); i++ {
		d.Pairs = append(d.Pairs, pairOf(reps[i-1], reps[i], names[i-1], names[i]))
	}
	d.Movers = movers(reps[0], reps[len(reps)-1])

	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, "compare.html", d); err != nil {
		return "", err
	}
	return b.String(), nil
}

func ratePct(r Rate) float64 {
	if r.Total == 0 {
		return 0
	}
	return 100 * float64(r.Passed) / float64(r.Total)
}

func tileOf(suite string, rs [][2]Rate, names []string) suiteTile {
	t := suiteTile{Suite: suite}
	first, last := -1, -1
	for i, r := range rs {
		if r[0].Total == 0 {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if last < 0 {
		return t
	}
	t.Strict, t.ValuesOnly, t.LastName = rs[last][0], rs[last][1], names[last]
	if first != last {
		t.HasDelta = true
		t.FirstName = names[first]
		t.FirstStrict = rs[first][0]
		t.StrictDelta = ratePct(rs[last][0]) - ratePct(rs[first][0])
		t.ValuesDelta = ratePct(rs[last][1]) - ratePct(rs[first][1])
	}
	return t
}

const (
	chartWidth, chartHeight = 320.0, 210.0
	plotLeft, plotRight     = 40.0, 264.0
	plotTop, plotBottom     = 14.0, 172.0
	// Approximate advance of an 11px x-axis label character, and the gap
	// kept between neighbouring labels.
	labelCharWidth, labelGap           = 6.5, 12.0
	moverMinCases, moverLimit, fixedBy = 10, 15, 15
)

func chartOf(suite string, rs [][2]Rate, names []string) trendChart {
	c := trendChart{Suite: suite, Width: chartWidth, Height: chartHeight, PlotLeft: plotLeft, PlotRight: plotRight}
	x := func(i int) float64 {
		if len(names) == 1 {
			return (plotLeft + plotRight) / 2
		}
		return plotLeft + (plotRight-plotLeft)*float64(i)/float64(len(names)-1)
	}
	y := func(pct float64) float64 { return plotBottom - (plotBottom-plotTop)*pct/100 }
	for _, tick := range []float64{0, 25, 50, 75, 100} {
		c.Grid = append(c.Grid, chartLabel{X: plotLeft - 8, Y: y(tick), Text: fmt.Sprintf("%.0f%%", tick), Anchor: "end"})
	}
	step := 1
	if len(names) > 1 {
		longest := 0
		for _, n := range names {
			longest = max(longest, len([]rune(n)))
		}
		spacing := (plotRight - plotLeft) / float64(len(names)-1)
		step = int(math.Ceil((float64(longest)*labelCharWidth + labelGap) / spacing))
	}
	for i, n := range names {
		// The last run is always labelled; a sampled label too close to it is dropped.
		last := len(names) - 1
		if i != last && (i%step != 0 || last-i < step) {
			continue
		}
		anchor := "middle"
		switch {
		case len(names) > 1 && i == 0:
			anchor = "start"
		case len(names) > 1 && i == len(names)-1:
			anchor = "end"
		}
		c.XLabels = append(c.XLabels, chartLabel{X: x(i), Y: plotBottom + 20, Text: n, Anchor: anchor})
	}
	var sp, vp strings.Builder
	for i, r := range rs {
		if r[0].Total == 0 {
			continue
		}
		ps := chartPoint{X: x(i), Y: y(ratePct(r[0])), Run: names[i], Series: "strict", Rate: r[0]}
		pv := chartPoint{X: x(i), Y: y(ratePct(r[1])), Run: names[i], Series: "ignoring __name__", Rate: r[1]}
		cmd := "L"
		if len(c.Strict) == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&sp, "%s%.1f %.1f ", cmd, ps.X, ps.Y)
		fmt.Fprintf(&vp, "%s%.1f %.1f ", cmd, pv.X, pv.Y)
		c.Strict = append(c.Strict, ps)
		c.Values = append(c.Values, pv)
	}
	c.StrictPath, c.ValuesPath = strings.TrimSpace(sp.String()), strings.TrimSpace(vp.String())
	if n := len(c.Strict); n > 0 {
		// The values-only rate is never below the strict one, so its label
		// goes above its point and the strict label below, which keeps them
		// apart when the two lines meet.
		s, v := c.Strict[n-1], c.Values[n-1]
		c.ValuesLabel = &chartLabel{X: v.X + 8, Y: v.Y - 6, Text: fmt.Sprintf("%.1f%%", ratePct(v.Rate)), Anchor: "start"}
		c.StrictLabel = &chartLabel{X: s.X + 8, Y: s.Y + 14, Text: fmt.Sprintf("%.1f%%", ratePct(s.Rate)), Anchor: "start"}
	}
	return c
}

func rowOf(c *result.Case) caseRow {
	r := caseRow{Suite: c.Suite, Group: c.Group, Expr: c.Expr, Status: c.Status}
	if c.Minimized != c.Expr {
		r.Minimized = c.Minimized
	}
	if ch := FirstFailure(c); ch != nil {
		r.Time, r.Detail = ch.Time, truncate(oneLine(ch.Detail), 300)
		if ch.Query != c.Expr {
			r.Query = ch.Query
		}
	}
	return r
}

func pairOf(base, cur *Report, from, to string) pairData {
	cmp := Compare(base, cur)
	p := pairData{From: from, To: to, ConfigDiff: base.Config.Diff(cur.Config), Suites: cmp.Suites}
	for _, c := range cmp.Regressed {
		p.Regressed = append(p.Regressed, rowOf(c))
	}
	groups := map[[2]string]int{}
	for _, c := range cmp.Fixed {
		p.Fixed = append(p.Fixed, rowOf(c))
		groups[[2]string{c.Suite, c.Group}]++
	}
	for k, n := range groups {
		p.FixedBy = append(p.FixedBy, groupCount{Suite: k[0], Group: k[1], N: n})
	}
	sort.Slice(p.FixedBy, func(i, j int) bool {
		a, b := p.FixedBy[i], p.FixedBy[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if a.Suite != b.Suite {
			return a.Suite < b.Suite
		}
		return a.Group < b.Group
	})
	if len(p.FixedBy) > fixedBy {
		p.FixedBy = p.FixedBy[:fixedBy]
	}
	return p
}

// constructTotals sums, per construct, passed and total in-scope cases over
// all suites of a run.
func constructTotals(rep *Report) map[string][2]int {
	names, m := rep.bySuite()
	out := map[string][2]int{}
	for _, r := range coverageTable(names, m) {
		var t [2]int
		for _, c := range r.Cells {
			if c != nil {
				t[0] += c.Passed
				t[1] += c.Total
			}
		}
		out[r.Construct] = t
	}
	return out
}

func movers(first, last *Report) []construct {
	a, b := constructTotals(first), constructTotals(last)
	var out []construct
	for name, fa := range a {
		lb, ok := b[name]
		if !ok || fa[1] < moverMinCases || lb[1] < moverMinCases {
			continue
		}
		delta := 100*float64(lb[0])/float64(lb[1]) - 100*float64(fa[0])/float64(fa[1])
		if math.Abs(delta) < 0.05 {
			continue
		}
		out = append(out, construct{Name: name, FirstPass: fa[0], FirstTotal: fa[1], LastPass: lb[0], LastTotal: lb[1], Delta: delta})
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].Delta) != math.Abs(out[j].Delta) {
			return math.Abs(out[i].Delta) > math.Abs(out[j].Delta)
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > moverLimit {
		out = out[:moverLimit]
	}
	return out
}
