package corpus

// Execution side of the forked promqltest runner. Upstream stops a file at the
// first failure and runs everything against an in-process engine; this runner
// replays each file's data into a fresh dataset on the target through remote
// write, sends every query over the HTTP API, and records every check.

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"

	"github.com/GreptimeTeam/promql-compat/internal/client"
	"github.com/GreptimeTeam/promql-compat/internal/result"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

//go:embed testdata
var testsFs embed.FS

// test is the parsed form of one .test file.
type test struct {
	cmds []testCommand
}

// Files returns the embedded upstream corpus files.
func Files() ([]string, error) {
	return fs.Glob(testsFs, "testdata/*.test")
}

// ReadFile returns an embedded corpus file.
func ReadFile(name string) (string, error) {
	b, err := fs.ReadFile(testsFs, name)
	return string(b), err
}

var (
	defaultParser = parser.NewParser(parser.Options{})
	// noDelayEngine evaluates with delayed __name__ removal disabled, to find
	// cases whose expectations depend on that feature flag.
	noDelayEngine = promql.NewEngine(promql.EngineOpts{
		MaxSamples:               DefaultMaxSamplesPerQuery,
		Timeout:                  100 * time.Second,
		NoStepSubqueryIntervalFn: func(int64) int64 { return time.Minute.Milliseconds() },
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		UseStartTimestamps:       true,
		Parser:                   parser.NewParser(TestParserOpts),
	})
	reSanitize = regexp.MustCompile(`[^a-z0-9_]+`)
)

// Runner replays corpus files against a target.
type Runner struct {
	Target target.Target
	RunID  string
	// KeepData leaves the created databases in place for debugging.
	KeepData     bool
	QueryTimeout time.Duration
}

// fileRun is the state while executing one file.
type fileRun struct {
	r       *Runner
	ctx     context.Context
	file    string
	st      *teststorage.TestStorage
	dirty   bool
	gen     int
	db      string
	dbs     []string
	ingest  error
	hist    []labels.Labels // series holding native histogram samples
	hasST   bool
	hasNHCB bool
	cases   []result.Case
	repro   string // data of the current dataset in load syntax
}

func newStorage() (*teststorage.TestStorage, error) {
	return teststorage.NewWithError(func(o *tsdb.Options) {
		o.EnableSTStorage = true
		o.FloatChunkEncoding = chunkenc.EncXOR2
		o.EnableHistogramSTEncoding = true
	})
}

// RunFile executes one corpus file and returns a case per eval command.
func (r *Runner) RunFile(ctx context.Context, name, content string) ([]result.Case, error) {
	t := &test{}
	if err := t.parse(content); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	st, err := newStorage()
	if err != nil {
		return nil, err
	}
	fr := &fileRun{r: r, ctx: ctx, file: path.Base(name), st: st, dirty: true}
	defer func() {
		_ = fr.st.Close()
		if !r.KeepData {
			for _, db := range fr.dbs {
				_ = r.Target.DropDB(context.Background(), db)
			}
		}
	}()
	var loads []string
	for _, cmd := range t.cmds {
		switch c := cmd.(type) {
		case *clearCmd:
			_ = fr.st.Close()
			if fr.st, err = newStorage(); err != nil {
				return nil, err
			}
			fr.dirty, fr.hasNHCB, fr.hasST = true, false, false
			loads, fr.repro = nil, ""
		case *loadCmd:
			app := fr.st.AppenderV2(ctx)
			if err := c.append(app); err != nil {
				_ = app.Rollback()
				return nil, fmt.Errorf("%s: load: %w", name, err)
			}
			if err := app.Commit(); err != nil {
				return nil, err
			}
			fr.dirty = true
			fr.hasNHCB = fr.hasNHCB || c.withNHCB
			for _, smpls := range c.defs {
				for _, s := range smpls {
					fr.hasST = fr.hasST || s.ST != 0
				}
			}
			loads = append(loads, c.source)
			fr.repro = strings.Join(loads, "\n\n")
		case *evalCmd:
			if fr.dirty {
				fr.sync()
			}
			fr.eval(c)
		}
	}
	return fr.cases, nil
}

// sync copies the local storage into a fresh database on the target.
func (fr *fileRun) sync() {
	fr.gen++
	fr.dirty = false
	fr.db = fmt.Sprintf("%s_%s_%d", fr.r.RunID, reSanitize.ReplaceAllString(strings.TrimSuffix(fr.file, ".test"), "_"), fr.gen)
	fr.dbs = append(fr.dbs, fr.db)
	fr.ingest = nil
	fr.hist = nil
	series, err := dump(fr.ctx, fr.st)
	if err != nil {
		fr.ingest = err
		return
	}
	for _, s := range series {
		if len(s.Histograms) > 0 {
			fr.hist = append(fr.hist, s.Labels)
		}
	}
	if err := fr.r.Target.CreateDB(fr.ctx, fr.db); err != nil {
		fr.ingest = fmt.Errorf("create database: %w", err)
		return
	}
	if err := fr.r.Target.Endpoint(fr.db).Write(fr.ctx, series); err != nil {
		fr.ingest = err
		return
	}
	if err := fr.r.Target.Ready(fr.ctx, fr.db); err != nil {
		fr.ingest = fmt.Errorf("ready: %w", err)
	}
}

// dump reads every series from s.
func dump(ctx context.Context, s storage.Queryable) ([]client.Series, error) {
	q, err := s.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	set := q.Select(ctx, true, nil, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".*"))
	var out []client.Series
	var it chunkenc.Iterator
	for set.Next() {
		ser := set.At()
		cs := client.Series{Labels: ser.Labels()}
		it = ser.Iterator(it)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			switch vt {
			case chunkenc.ValFloat:
				t, f := it.At()
				cs.Floats = append(cs.Floats, promql.FPoint{T: t, F: f})
			case chunkenc.ValHistogram, chunkenc.ValFloatHistogram:
				t, h := it.AtFloatHistogram(nil)
				cs.Histograms = append(cs.Histograms, promql.HPoint{T: t, H: h})
			}
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, set.Err()
}

func (fr *fileRun) tags(cmd *evalCmd) []string {
	var tags []string
	expr, err := parserForBuiltinTests.ParseExpr(cmd.expr)
	if err == nil && fr.touchesHistogram(expr) {
		tags = append(tags, result.TagNativeHistogram)
	} else if fr.hasNHCB || expectsHistogram(cmd) {
		tags = append(tags, result.TagNativeHistogram)
	}
	if fr.hasST {
		tags = append(tags, result.TagStartTimestamp)
	}
	if _, err := defaultParser.ParseExpr(cmd.expr); err != nil {
		tags = append(tags, result.TagExperimental)
	}
	if fr.dependsOnDelayedNameRemoval(cmd) {
		tags = append(tags, result.TagDelayedName)
	}
	return tags
}

func expectsHistogram(cmd *evalCmd) bool {
	for _, e := range cmd.expected {
		for _, v := range e.vals {
			if v.Histogram != nil {
				return true
			}
		}
	}
	return false
}

// touchesHistogram reports whether any selector in expr matches a series
// holding native histogram samples.
func (fr *fileRun) touchesHistogram(expr parser.Expr) bool {
	if len(fr.hist) == 0 {
		return false
	}
	found := false
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok || found {
			return nil
		}
		for _, h := range fr.hist {
			all := true
			for _, m := range vs.LabelMatchers {
				if !m.Matches(h.Get(m.Name)) {
					all = false
					break
				}
			}
			if all {
				found = true
				return nil
			}
		}
		return nil
	})
	return found
}

// dependsOnDelayedNameRemoval evaluates the base query locally with delayed
// __name__ removal off; if the expectation no longer holds, the case only
// applies to servers running with that feature flag.
func (fr *fileRun) dependsOnDelayedNameRemoval(cmd *evalCmd) bool {
	var (
		q   promql.Query
		err error
	)
	if cmd.isRange {
		q, err = noDelayEngine.NewRangeQuery(fr.ctx, fr.st, nil, cmd.expr, cmd.start, cmd.end, cmd.step)
	} else {
		q, err = noDelayEngine.NewInstantQuery(fr.ctx, fr.st, nil, cmd.expr, cmd.eval)
	}
	if err != nil {
		return false
	}
	defer q.Close()
	res := q.Exec(fr.ctx)
	if res.Err != nil {
		return !cmd.isFail()
	}
	if cmd.isFail() {
		return true
	}
	if m, ok := res.Value.(promql.Matrix); ok {
		sort.Sort(m)
	}
	return cmd.compareResult(res.Value) != nil
}

func (fr *fileRun) eval(cmd *evalCmd) {
	c := result.Case{
		Suite: "corpus",
		ID:    fmt.Sprintf("%s:%d", fr.file, cmd.line),
		Group: fr.file,
		Expr:  cmd.expr,
		Tags:  fr.tags(cmd),
		Repro: fr.repro,
	}
	defer func() {
		c.Finish()
		fr.cases = append(fr.cases, c)
	}()
	if fr.ingest != nil {
		c.Checks = append(c.Checks, result.Check{Kind: "ingest", Status: result.FailIngest, Detail: fr.ingest.Error()})
		return
	}
	if cmd.isRange {
		c.Checks = append(c.Checks, fr.checkRange(cmd))
		return
	}
	queries, err := atModifierTestCases(cmd.expr, cmd.eval)
	if err != nil {
		c.Checks = append(c.Checks, result.Check{Kind: "instant", Query: cmd.expr, Status: result.FailTransport, Detail: err.Error()})
		return
	}
	queries = append([]atModifierTestCase{{expr: cmd.expr, evalTime: cmd.eval}}, queries...)
	for i, iq := range queries {
		kind := "instant"
		if i > 0 {
			kind = "instant_at"
		}
		c.Checks = append(c.Checks, fr.checkInstant(cmd, iq, kind)...)
	}
}

func (fr *fileRun) queryCtx() (context.Context, context.CancelFunc) {
	d := fr.r.QueryTimeout
	if d == 0 {
		d = 60 * time.Second
	}
	return context.WithTimeout(fr.ctx, d)
}

// classifyErr maps a failed query to a check status.
func classifyErr(cmd *evalCmd, err error) (result.Status, string) {
	var qe *client.QueryError
	if !errors.As(err, &qe) {
		return result.FailTransport, err.Error()
	}
	if !cmd.isFail() {
		return result.FailError, err.Error()
	}
	if ferr := cmd.checkExpectedFailure(err); ferr != nil {
		return result.PassErrorMessage, ferr.Error()
	}
	return result.Pass, ""
}

// compare checks val against the expectation; an unsorted matrix is sorted
// and reported as PassUnsorted if the values match.
func compare(cmd *evalCmd, val parser.Value) (result.Status, string) {
	status := result.Pass
	detail := ""
	if m, ok := val.(promql.Matrix); ok && !cmd.isOrdered() {
		if err := assertMatrixSorted(m); err != nil {
			sorted := append(promql.Matrix(nil), m...)
			sort.Sort(sorted)
			val = sorted
			status, detail = result.PassUnsorted, err.Error()
		}
	}
	if err := cmd.compareResult(val); err != nil {
		return result.FailValue, err.Error()
	}
	return status, detail
}

// passesWithoutName reports whether val matches the expectation once
// __name__ is removed from both, i.e. the only difference is the metric name.
func passesWithoutName(cmd *evalCmd, val parser.Value) bool {
	c := *cmd
	c.metrics = map[uint64]labels.Labels{}
	c.expected = map[uint64]entry{}
	for h, m := range cmd.metrics {
		m2 := m.DropMetricName()
		c.metrics[m2.Hash()] = m2
		c.expected[m2.Hash()] = cmd.expected[h]
	}
	if cmd.expectScalar {
		c.expected = cmd.expected
	}
	switch v := val.(type) {
	case promql.Vector:
		out := make(promql.Vector, len(v))
		for i, s := range v {
			s.Metric = s.Metric.DropMetricName()
			out[i] = s
		}
		val = out
	case promql.Matrix:
		out := make(promql.Matrix, len(v))
		for i, s := range v {
			s.Metric = s.Metric.DropMetricName()
			out[i] = s
		}
		sort.Sort(out)
		val = out
	}
	return c.compareResult(val) == nil
}

func (fr *fileRun) annotations(cmd *evalCmd, expr string, res *client.Result) string {
	if err := checkAnnotationStrings(cmd, expr, res.Warnings, res.Infos); err != nil {
		return err.Error()
	}
	return ""
}

func (fr *fileRun) checkRange(cmd *evalCmd) result.Check {
	ch := result.Check{Kind: "range", Query: cmd.expr,
		Time: fmt.Sprintf("%s..%s/%s", client.FormatTime(cmd.start.UnixMilli()), client.FormatTime(cmd.end.UnixMilli()), cmd.step)}
	ctx, cancel := fr.queryCtx()
	defer cancel()
	res, err := fr.r.Target.Endpoint(fr.db).Range(ctx, cmd.expr, cmd.start, cmd.end, cmd.step)
	if err != nil {
		ch.Status, ch.Detail = classifyErr(cmd, err)
		if ch.Status.Passed() {
			ch.Evidence = result.EvidenceError
		}
		return ch
	}
	if cmd.isFail() {
		ch.Status, ch.Detail = result.FailNoError, "expected an error, got "+truncate(res.Value.String(), 500)
		return ch
	}
	ch.Annotation = fr.annotations(cmd, cmd.expr, res)
	ch.Status, ch.Detail = compare(cmd, res.Value)
	if ch.Status.Passed() {
		ch.Evidence = result.EvidenceOf(res.Empty())
	}
	if ch.Status == result.FailValue && passesWithoutName(cmd, res.Value) {
		ch.Cause = result.CauseMetricName
	}
	return ch
}

func (fr *fileRun) checkInstant(cmd *evalCmd, iq atModifierTestCase, kind string) []result.Check {
	ch := result.Check{Kind: kind, Query: iq.expr, Time: client.FormatTime(iq.evalTime.UnixMilli())}
	ctx, cancel := fr.queryCtx()
	defer cancel()
	ep := fr.r.Target.Endpoint(fr.db)
	res, err := ep.Instant(ctx, iq.expr, iq.evalTime)
	if err != nil {
		ch.Status, ch.Detail = classifyErr(cmd, err)
		if ch.Status.Passed() {
			ch.Evidence = result.EvidenceError
		}
		return []result.Check{ch}
	}
	if cmd.isFail() {
		ch.Status, ch.Detail = result.FailNoError, "expected an error, got "+truncate(res.Value.String(), 500)
		return []result.Check{ch}
	}
	ch.Annotation = fr.annotations(cmd, iq.expr, res)
	ch.Status, ch.Detail = compare(cmd, res.Value)
	if ch.Status.Passed() {
		ch.Evidence = result.EvidenceOf(res.Empty())
	}
	if ch.Status == result.FailValue && passesWithoutName(cmd, res.Value) {
		ch.Cause = result.CauseMetricName
	}
	out := []result.Check{ch}

	if cmd.excludeFromRangeQuery || reQueryContextFuncs.MatchString(iq.expr) {
		return out
	}
	rm := result.Check{Kind: "range_mode", Query: iq.expr, Time: ch.Time + "±1m/1m"}
	rres, err := ep.Range(ctx, iq.expr, iq.evalTime.Add(-time.Minute), iq.evalTime.Add(time.Minute), time.Minute)
	if err != nil {
		var qe *client.QueryError
		rm.Status, rm.Detail = result.FailError, err.Error()
		if !errors.As(err, &qe) {
			rm.Status = result.FailTransport
		}
		return append(out, rm)
	}
	if cmd.isOrdered() {
		return out
	}
	mat, ok := rres.Value.(promql.Matrix)
	if !ok {
		rm.Status, rm.Detail = result.FailValue, fmt.Sprintf("range query returned %s, want matrix", rres.Value.Type())
		return append(out, rm)
	}
	rm.Status = result.Pass
	if err := assertMatrixSorted(mat); err != nil {
		rm.Status, rm.Detail = result.PassUnsorted, err.Error()
		mat = append(promql.Matrix(nil), mat...)
		sort.Sort(mat)
	}
	vec := make(promql.Vector, 0, len(mat))
	evalMs := iq.evalTime.UnixMilli()
	for _, s := range mat {
		for _, p := range s.Floats {
			if p.T == evalMs {
				vec = append(vec, promql.Sample{Metric: s.Metric, T: p.T, F: p.F})
				break
			}
		}
		for _, p := range s.Histograms {
			if p.T == evalMs {
				vec = append(vec, promql.Sample{Metric: s.Metric, T: p.T, H: p.H})
				break
			}
		}
	}
	var cerr error
	if _, isScalar := res.Value.(promql.Scalar); isScalar {
		if len(vec) == 0 {
			cerr = errors.New("range mode returned no sample at the evaluation time")
		} else {
			cerr = cmd.compareResult(promql.Scalar{V: vec[0].F})
		}
	} else {
		cerr = cmd.compareResult(vec)
	}
	if cerr == nil {
		rm.Evidence = result.EvidenceOf(len(vec) == 0)
	} else {
		rm.Status, rm.Detail = result.FailValue, cerr.Error()
		if _, isScalar := res.Value.(promql.Scalar); !isScalar && passesWithoutName(cmd, vec) {
			rm.Cause = result.CauseMetricName
		}
	}
	return append(out, rm)
}

// checkAnnotationStrings mirrors checkAnnotations for annotations received as
// strings over the HTTP API.
func checkAnnotationStrings(ev *evalCmd, expr string, warnings, infos []string) error {
	switch {
	case ev.warn && len(warnings) == 0:
		return fmt.Errorf("expected warnings evaluating query %q (line %d) but got none", expr, ev.line)
	case ev.info && len(infos) == 0:
		return fmt.Errorf("expected info annotations evaluating query %q (line %d) but got none", expr, ev.line)
	}
	if err := validateExpectedAnnotationsOfType(expr, ev.expectedCmds[Warn], warnings, ev.line, "warn"); err != nil {
		return err
	}
	if err := validateExpectedAnnotationsOfType(expr, ev.expectedCmds[Info], infos, ev.line, "info"); err != nil {
		return err
	}
	if ev.expectedCmds[NoWarn] != nil && len(warnings) > 0 {
		return fmt.Errorf("unexpected warning annotations evaluating query %q (line %d): %v", expr, ev.line, warnings)
	}
	if ev.expectedCmds[NoInfo] != nil && len(infos) > 0 {
		return fmt.Errorf("unexpected info annotations evaluating query %q (line %d): %v", expr, ev.line, infos)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// LoadSeries applies the load commands of a promqltest snippet and returns
// the resulting series; eval commands are ignored. Used for ad-hoc repros.
func LoadSeries(ctx context.Context, content string) ([]client.Series, error) {
	t := &test{}
	if err := t.parse(content); err != nil {
		return nil, err
	}
	st, err := newStorage()
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	for _, cmd := range t.cmds {
		switch c := cmd.(type) {
		case *clearCmd:
			fresh, err := newStorage()
			if err != nil {
				return nil, err
			}
			_ = st.Close()
			st = fresh
		case *loadCmd:
			app := st.AppenderV2(ctx)
			if err := c.append(app); err != nil {
				_ = app.Rollback()
				return nil, err
			}
			if err := app.Commit(); err != nil {
				return nil, err
			}
		}
	}
	return dump(ctx, st)
}
