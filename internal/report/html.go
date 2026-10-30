package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"sort"
	"strings"
	"time"
)

//go:embed report.html
var htmlSource string

var htmlTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"pct": func(n, total int) float64 {
		if total == 0 {
			return 0
		}
		return 100 * float64(n) / float64(total)
	},
}).Parse(htmlSource))

type htmlSuite struct {
	Name                               string
	Strict, Primary, NegTS, ValuesOnly Rate
	// NameOnly and Other split the failed in-scope cases: those that pass
	// when __name__ differences are ignored, and the rest.
	NameOnly, Other int
	Excluded        int
	Evidence        evidenceCounts
	Causes          []causeCount
	Signatures      []signature
	Tags            []tagCount
}

type htmlConstruct struct {
	Construct     string
	Passed, Total int
	Cells         []*coverageCell
}

type htmlData struct {
	Target, Label, Build string
	Reference, Harness   string
	Suites               []string
	Files                string
	FuzzCount            int
	FuzzSeed             int64
	Command              string
	Started              string
	Duration             time.Duration
	Rows                 []htmlSuite
	Weakest              []htmlConstruct
	Coverage             []coverageRow
}

// Constructs with fewer in-scope cases than this over all suites are left out
// of the weakest list; their rate says little.
const weakestMinCases = 10

// HTML renders a one-page summary of the run. REPORT.md stays the complete
// record, including every failure.
func (rep *Report) HTML() (string, error) {
	names, m := rep.bySuite()
	d := htmlData{
		Target:    rep.Target,
		Label:     rep.Label,
		Build:     buildLine(rep.Version),
		Reference: orUnknown(rep.Config.Reference),
		Harness:   shortRevision(rep.Config.Harness),
		Suites:    names,
		Files:     orAll(rep.Config.Files),
		FuzzCount: rep.Config.FuzzCount,
		FuzzSeed:  rep.Config.FuzzSeed,
		Command:   oneLine(strings.Join(rep.Config.Args, " ")),
		Started:   rep.Started.Format("2006-01-02 15:04 MST"),
		Duration:  rep.Finished.Sub(rep.Started).Round(time.Second),
		Coverage:  coverageTable(names, m),
	}
	for _, n := range names {
		cases := m[n]
		v := views(cases)
		d.Rows = append(d.Rows, htmlSuite{
			Name:       n,
			Strict:     v[0],
			Primary:    v[1],
			NegTS:      v[2],
			ValuesOnly: v[3],
			NameOnly:   v[3].Passed - v[0].Passed,
			Other:      v[3].Total - v[3].Passed,
			Excluded:   rateOf(cases).Excluded,
			Evidence:   evidenceOf(cases),
			Causes:     causeCounts(cases),
			Signatures: signatures(cases, 10),
			Tags:       tagCounts(cases),
		})
	}
	for _, r := range d.Coverage {
		w := htmlConstruct{Construct: r.Construct, Cells: r.Cells}
		for _, c := range r.Cells {
			if c != nil {
				w.Passed += c.Passed
				w.Total += c.Total
			}
		}
		if w.Total >= weakestMinCases {
			d.Weakest = append(d.Weakest, w)
		}
	}
	sort.SliceStable(d.Weakest, func(i, j int) bool {
		a, b := d.Weakest[i], d.Weakest[j]
		return a.Passed*b.Total < b.Passed*a.Total
	})
	if len(d.Weakest) > 15 {
		d.Weakest = d.Weakest[:15]
	}
	var b bytes.Buffer
	if err := htmlTemplate.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// buildLine shortens GreptimeDB's build-info JSON to "version (commit)";
// other targets report plain text, kept as is.
func buildLine(version string) string {
	var bi struct {
		Data struct {
			Version     string `json:"version"`
			CommitShort string `json:"commit_short"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(version), &bi) == nil && bi.Data.Version != "" {
		if bi.Data.CommitShort != "" {
			return bi.Data.Version + " (" + bi.Data.CommitShort + ")"
		}
		return bi.Data.Version
	}
	return oneLine(version)
}

// shortRevision abbreviates a full commit hash, keeping a "+dirty" suffix.
func shortRevision(rev string) string {
	if rev == "" {
		return "unknown"
	}
	hash, dirty, _ := strings.Cut(rev, "+")
	if len(hash) > 7 {
		hash = hash[:7]
	}
	if dirty != "" {
		return hash + "+" + dirty
	}
	return hash
}
