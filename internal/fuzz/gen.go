// Package fuzz generates random, type-correct PromQL over the demo dataset,
// runs it differentially and shrinks failing queries to minimal repros.
package fuzz

import (
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GreptimeTeam/promql-compat/internal/datagen"
)

type metric struct {
	name    string
	counter bool
	labels  map[string][]string // label -> sample values
}

var common = map[string][]string{
	"instance": {"demo.promlabs.com:10000", "demo.promlabs.com:10001", "demo.promlabs.com:10002"},
	"job":      {"demo"},
}

func with(extra map[string][]string) map[string][]string {
	m := map[string][]string{}
	for k, v := range common {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var metrics = []metric{
	{"demo_memory_usage_bytes", false, with(map[string][]string{"type": {"used", "cached", "buffers", "free"}})},
	{"demo_cpu_usage_seconds_total", true, with(map[string][]string{"mode": {"idle", "system", "user"}})},
	{"demo_num_cpus", false, with(nil)},
	{"demo_disk_usage_bytes", false, with(nil)},
	{"demo_disk_total_bytes", false, with(nil)},
	{"demo_items_shipped_total", true, with(nil)},
	{"demo_api_request_duration_seconds_count", true, with(map[string][]string{
		"method": {"GET", "POST"}, "path": {"/api/foo", "/api/bar"}, "status": {"200", "500"}})},
	{"demo_api_request_duration_seconds_bucket", true, with(map[string][]string{
		"method": {"GET", "POST"}, "path": {"/api/foo", "/api/bar"}, "status": {"200", "500"}, "le": {"0.0001", "0.0038443359375", "+Inf"}})},
	{"demo_intermittent_metric", false, with(nil)},
	{"up", false, with(nil)},
	{"demo_batch_last_success_timestamp_seconds", false, with(nil)},
	{"nonexistent_metric", false, with(nil)},
}

var (
	ranges     = []string{"15s", "1m", "90s", "5m", "10m"}
	steps      = []string{"10s", "30s", "1m"}
	offsets    = []string{"30s", "1m", "5m", "-1m"}
	rangeFuncs = []string{"rate", "irate", "increase", "delta", "idelta", "deriv", "changes", "resets",
		"avg_over_time", "min_over_time", "max_over_time", "sum_over_time", "count_over_time",
		"last_over_time", "stddev_over_time", "stdvar_over_time", "present_over_time", "absent_over_time",
		"first_over_time", "quantile_over_time", "predict_linear"}
	counterFuncs = map[string]bool{"rate": true, "irate": true, "increase": true, "resets": true}
	mathFuncs    = []string{"abs", "ceil", "floor", "round", "exp", "ln", "log2", "log10", "sqrt", "sgn",
		"timestamp", "sort", "sort_desc", "absent", "-",
		"sin", "cos", "tan", "asin", "acos", "atan", "sinh", "cosh", "tanh", "asinh", "acosh", "atanh", "deg", "rad",
		"day_of_month", "day_of_week", "day_of_year", "days_in_month", "hour", "minute", "month", "year"}
	aggrs      = []string{"sum", "avg", "min", "max", "count", "stddev", "stdvar", "group"}
	arithOps   = []string{"+", "-", "*", "/", "%", "^", "atan2"}
	compOps    = []string{"==", "!=", ">", "<", ">=", "<="}
	setOps     = []string{"and", "or", "unless"}
	scalarLits = []string{"0", "1", "2", "0.5", "-1", "10", "1e9", "NaN", "Inf"}
)

// Gen is a random query generator.
type Gen struct {
	r *rand.Rand
}

func NewGen(seed int64) *Gen { return &Gen{r: rand.New(rand.NewSource(seed))} }

func (g *Gen) pick(xs []string) string { return xs[g.r.Intn(len(xs))] }
func (g *Gen) chance(p float64) bool   { return g.r.Float64() < p }

// Query returns a random instant-vector or scalar query.
func (g *Gen) Query() string {
	if g.chance(0.1) {
		return g.scalar(2)
	}
	return g.vector(3)
}

func (g *Gen) metric(counter bool) metric {
	for {
		m := metrics[g.r.Intn(len(metrics))]
		if !counter || m.counter {
			return m
		}
	}
}

func (g *Gen) selector(m metric) string {
	var ms []string
	for _, l := range sortedKeys(m.labels) {
		if !g.chance(0.25) {
			continue
		}
		v := g.pick(m.labels[l])
		switch g.r.Intn(4) {
		case 0:
			ms = append(ms, fmt.Sprintf("%s=%q", l, v))
		case 1:
			ms = append(ms, fmt.Sprintf("%s!=%q", l, v))
		case 2:
			ms = append(ms, fmt.Sprintf("%s=~%q", l, regexOf(v)))
		case 3:
			ms = append(ms, fmt.Sprintf("%s!~%q", l, regexOf(v)))
		}
	}
	sortStrings(ms)
	s := m.name
	if g.chance(0.05) {
		ms = append([]string{fmt.Sprintf("__name__=%q", m.name)}, ms...)
		s = ""
	}
	if len(ms) > 0 {
		s += "{" + strings.Join(ms, ", ") + "}"
	}
	return s
}

func regexOf(v string) string {
	if len(v) > 3 {
		return regexp.QuoteMeta(v[:len(v)-2]) + ".*"
	}
	return regexp.QuoteMeta(v) + "|x"
}

func (g *Gen) modifiers() string {
	s := ""
	if g.chance(0.12) {
		s += " offset " + g.pick(offsets)
	}
	if g.chance(0.06) {
		// A fixed timestamp inside the demo dataset, or the query bounds.
		at := strconv.FormatInt(datagen.DemoStart.Add(60*time.Minute).Unix(), 10)
		s += " @ " + g.pick([]string{"start()", "end()", at})
	}
	return s
}

func (g *Gen) matrix(counter bool) string {
	return g.selector(g.metric(counter)) + "[" + g.pick(ranges) + "]" + g.modifiers()
}

func (g *Gen) grouping() string {
	if g.chance(0.4) {
		return ""
	}
	labels := []string{"instance", "job", "type", "mode", "method", "status", "le", "nonexistent"}
	var ls []string
	for _, l := range labels {
		if g.chance(0.25) {
			ls = append(ls, l)
		}
	}
	kw := "by"
	if g.chance(0.35) {
		kw = "without"
	}
	return fmt.Sprintf(" %s (%s)", kw, strings.Join(ls, ", "))
}

func (g *Gen) vector(depth int) string {
	if depth <= 0 {
		return g.selector(g.metric(false)) + g.modifiers()
	}
	switch g.r.Intn(11) {
	case 0, 1:
		return g.selector(g.metric(false)) + g.modifiers()
	case 2, 3:
		switch f := g.pick(rangeFuncs); f {
		case "quantile_over_time":
			return fmt.Sprintf("%s(%s, %s)", f, g.pick([]string{"0", "0.5", "0.9", "1"}), g.matrix(false))
		case "predict_linear":
			return fmt.Sprintf("%s(%s, %s)", f, g.matrix(false), g.pick([]string{"60", "600", "-300"}))
		default:
			return fmt.Sprintf("%s(%s)", f, g.matrix(counterFuncs[f]))
		}
	case 4:
		f := g.pick(mathFuncs)
		if f == "-" {
			return "-" + g.paren(g.vector(depth-1))
		}
		return fmt.Sprintf("%s(%s)", f, g.vector(depth-1))
	case 5:
		a := g.pick(aggrs)
		return fmt.Sprintf("%s%s (%s)", a, g.grouping(), g.vector(depth-1))
	case 6:
		switch g.r.Intn(8) {
		case 0:
			return fmt.Sprintf("%s%s (%d, %s)", g.pick([]string{"topk", "bottomk"}), g.grouping(), 1+g.r.Intn(3), g.vector(depth-1))
		case 1:
			return fmt.Sprintf("quantile%s (%s, %s)", g.grouping(), g.pick([]string{"0", "0.5", "0.9", "1"}), g.vector(depth-1))
		case 2:
			return fmt.Sprintf("count_values%s (\"v\", %s)", g.grouping(), g.vector(depth-1))
		case 3:
			return fmt.Sprintf("clamp(%s, %s, %s)", g.vector(depth-1), g.pick(scalarLits[:6]), g.pick([]string{"1", "10", "1e9"}))
		case 4:
			return fmt.Sprintf("%s(%s, %s)", g.pick([]string{"clamp_min", "clamp_max"}), g.vector(depth-1), g.pick(scalarLits[:6]))
		case 5:
			return fmt.Sprintf("label_join(%s, \"dst\", \"-\", \"instance\", \"job\")", g.vector(depth-1))
		case 6:
			return fmt.Sprintf("vector(%s)", g.scalar(depth-1))
		default:
			return fmt.Sprintf("label_replace(%s, \"dst\", \"$1\", \"instance\", \"(.*):.*\")", g.vector(depth-1))
		}
	case 7:
		// vector op scalar, either side, possibly non-literal scalar.
		op := g.pick(append(append([]string{}, arithOps...), compOps...))
		sc := g.scalar(1)
		boolKw := ""
		if isComp(op) && g.chance(0.5) {
			boolKw = " bool"
		}
		if g.chance(0.5) {
			return fmt.Sprintf("%s %s%s %s", g.paren(g.vector(depth-1)), op, boolKw, sc)
		}
		return fmt.Sprintf("%s %s%s %s", sc, op, boolKw, g.paren(g.vector(depth-1)))
	case 8:
		return g.vectorBinary(depth)
	case 9:
		f := g.pick([]string{"max_over_time", "avg_over_time", "rate", "min_over_time", "sum_over_time"})
		inner := g.vector(depth - 1)
		if f == "rate" {
			inner = g.selector(g.metric(true))
		}
		return fmt.Sprintf("%s(%s[%s:%s])%s", f, g.paren(inner), g.pick(ranges[1:]), g.pick(steps), "")
	default:
		return fmt.Sprintf("histogram_quantile(%s, sum by (le%s) (rate(%s[%s])))",
			g.pick([]string{"0.5", "0.9", "0.99"}), g.pick([]string{"", ", instance", ", method"}),
			g.selector(metrics[7]), g.pick(ranges[1:]))
	}
}

func isComp(op string) bool {
	for _, c := range compOps {
		if c == op {
			return true
		}
	}
	return false
}

// vectorBinary combines two vectors of the same metric shape so that
// matching usually succeeds.
func (g *Gen) vectorBinary(depth int) string {
	m := g.metric(false)
	lhs := g.selector(m)
	rhs := g.selector(m)
	if g.chance(0.3) {
		lhs = fmt.Sprintf("%s(%s)", g.pick([]string{"abs", "ceil", "timestamp"}), lhs)
	}
	var op string
	switch g.r.Intn(3) {
	case 0:
		op = g.pick(arithOps)
	case 1:
		op = g.pick(compOps)
		if g.chance(0.4) {
			op += " bool"
		}
	default:
		op = g.pick(setOps)
	}
	matching := ""
	if g.chance(0.4) {
		kw := "on"
		if g.chance(0.4) {
			kw = "ignoring"
		}
		var ls []string
		for _, l := range sortedKeys(m.labels) {
			if g.chance(0.5) {
				ls = append(ls, l)
			}
		}
		matching = fmt.Sprintf(" %s (%s)", kw, strings.Join(ls, ", "))
	}
	if !strings.HasPrefix(op, "and") && !strings.HasPrefix(op, "or") && !strings.HasPrefix(op, "unless") && g.chance(0.25) {
		// many-to-one against an aggregate on the right.
		agg := fmt.Sprintf("%s by (instance) (%s)", g.pick([]string{"sum", "max", "count"}), rhs)
		if g.chance(0.5) {
			return fmt.Sprintf("%s %s on (instance) group_right %s", agg, op, lhs)
		}
		return fmt.Sprintf("%s %s on (instance) group_left %s", lhs, op, agg)
	}
	return fmt.Sprintf("%s %s%s %s", lhs, op, matching, rhs)
}

func (g *Gen) scalar(depth int) string {
	switch g.r.Intn(5) {
	case 0, 1:
		return g.pick(scalarLits)
	case 2:
		return g.pick([]string{"time()", "pi()"})
	case 3:
		if depth > 0 {
			return fmt.Sprintf("scalar(%s)", g.vector(depth-1))
		}
		return g.pick(scalarLits)
	default:
		if depth > 0 {
			return fmt.Sprintf("(%s %s %s)", g.scalar(depth-1), g.pick(arithOps), g.scalar(depth-1))
		}
		return "time()"
	}
}

func (g *Gen) paren(s string) string {
	if strings.ContainsAny(s, " ") || strings.HasPrefix(s, "-") {
		return "(" + s + ")"
	}
	return s
}

func sortedKeys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return ks
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
