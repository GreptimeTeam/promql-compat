// Package datagen produces deterministic datasets shaped like what a real
// Prometheus scrape produces: jittered millisecond timestamps, counters with
// resets, classic histograms, targets going down (staleness markers), and a
// series that appears and disappears.
package datagen

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql"

	"github.com/GreptimeTeam/promql-compat/internal/client"
)

// Dataset is a set of series covering [Start, End].
type Dataset struct {
	Series     []client.Series
	Start, End time.Time
}

// Samples returns the total number of samples.
func (d *Dataset) Samples() int {
	n := 0
	for _, s := range d.Series {
		n += len(s.Floats)
	}
	return n
}

// DemoStart crosses a month boundary so date functions see two months.
var DemoStart = time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC)

const (
	scrapeInterval = 5 * time.Second
	demoDuration   = 2 * time.Hour
)

// Instances matches the targets used by the compliance query templates.
var Instances = []string{"demo.promlabs.com:10000", "demo.promlabs.com:10001", "demo.promlabs.com:10002"}

type apiRoute struct {
	method, path string
	latency      float64 // seconds
	errorRatio   float64
	outage       time.Duration
}

var routes = []apiRoute{
	{"GET", "/api/foo", 0.010, 0.005, 23 * time.Second},
	{"POST", "/api/foo", 0.020, 0.02, time.Minute},
	{"GET", "/api/bar", 0.015, 0.0025, 13 * time.Second},
	{"POST", "/api/bar", 0.050, 0.01, 47 * time.Second},
}

// histogram buckets of the demo service: ExponentialBuckets(0.0001, 1.5, 25).
var buckets = func() []float64 {
	b := make([]float64, 25)
	v := 0.0001
	for i := range b {
		b[i] = v
		v *= 1.5
	}
	return b
}()

// builder accumulates samples per series in scrape order.
type builder struct {
	series map[string]*client.Series
	order  []string
}

func (b *builder) add(l labels.Labels, t int64, v float64) {
	k := l.String()
	s, ok := b.series[k]
	if !ok {
		s = &client.Series{Labels: l}
		b.series[k] = s
		b.order = append(b.order, k)
	}
	s.Floats = append(s.Floats, promql.FPoint{T: t, F: v})
}

// instanceState is what one demo process exposes.
type instanceState struct {
	cpus          int
	cpu           map[string]float64 // mode -> seconds
	shipped       float64
	hist          map[string][]float64 // route/status -> cumulative bucket counts (+Inf last)
	sum, count    map[string]float64
	lastBatch     float64
	lastSuccess   float64
	batchDuration float64
	batchBytes    float64
}

func newInstanceState(cpus int) *instanceState {
	return &instanceState{
		cpus: cpus, cpu: map[string]float64{},
		hist: map[string][]float64{}, sum: map[string]float64{}, count: map[string]float64{},
	}
}

// Demo generates two hours of scrapes of three demo-service instances.
// Instance :10002 is down between minute 50 and 55; instance :10001 restarts
// at minute 70, resetting its counters.
func Demo(seed int64) *Dataset {
	rng := rand.New(rand.NewSource(seed))
	b := &builder{series: map[string]*client.Series{}}
	start := DemoStart
	end := start.Add(demoDuration)
	cpus := []int{4, 8, 2}
	offsets := []time.Duration{0, 1234 * time.Millisecond, 2718 * time.Millisecond}
	states := make([]*instanceState, len(Instances))
	for i := range states {
		states[i] = newInstanceState(cpus[i])
	}

	for i, inst := range Instances {
		base := labels.FromStrings("instance", inst, "job", "demo")
		with := func(name string, kv ...string) labels.Labels {
			lb := labels.NewBuilder(base)
			lb.Set(labels.MetricName, name)
			for j := 0; j+1 < len(kv); j += 2 {
				lb.Set(kv[j], kv[j+1])
			}
			return lb.Labels()
		}
		st := states[i]
		var exposed map[string]labels.Labels // series exposed in the previous scrape
		for ts := start.Add(offsets[i]); !ts.After(end); ts = ts.Add(scrapeInterval) {
			// Scrape timestamps jitter by a few milliseconds, as in a real Prometheus.
			t := ts.UnixMilli() + int64(rng.Intn(4))
			elapsed := ts.Sub(start)
			down := i == 2 && elapsed >= 50*time.Minute && elapsed < 55*time.Minute
			if i == 1 && elapsed >= 70*time.Minute && elapsed < 70*time.Minute+scrapeInterval {
				*st = *newInstanceState(st.cpus)
			}
			b.add(with("up"), t, boolf(!down))
			if down {
				// A failed scrape marks every previously exposed series stale.
				for _, l := range exposed {
					b.add(l, t, math.Float64frombits(value.StaleNaN))
				}
				exposed = nil
				continue
			}
			cur := map[string]labels.Labels{}
			emit := func(l labels.Labels, v float64) {
				b.add(l, t, v)
				cur[l.String()] = l
			}
			secs := elapsed.Seconds()
			phase := float64(i)

			emit(with("demo_num_cpus"), float64(st.cpus))

			total := 16e9 + 4e9*phase
			used := math.Round(total * (0.4 + 0.1*math.Sin(secs/600+phase) + 0.02*rng.Float64()))
			cached := math.Round(total * (0.2 + 0.05*math.Cos(secs/900+phase)))
			buffers := math.Round(total * 0.05)
			emit(with("demo_memory_usage_bytes", "type", "used"), used)
			emit(with("demo_memory_usage_bytes", "type", "cached"), cached)
			emit(with("demo_memory_usage_bytes", "type", "buffers"), buffers)
			emit(with("demo_memory_usage_bytes", "type", "free"), total-used-cached-buffers)

			load := 0.5 + 0.4*math.Sin(secs/300+phase)
			step := scrapeInterval.Seconds() * float64(st.cpus)
			st.cpu["user"] += step * load * 0.7 * (0.9 + 0.2*rng.Float64())
			st.cpu["system"] += step * load * 0.3 * (0.9 + 0.2*rng.Float64())
			st.cpu["idle"] += step * (1 - load)
			for _, mode := range []string{"idle", "system", "user"} {
				emit(with("demo_cpu_usage_seconds_total", "mode", mode), round3(st.cpu[mode]))
			}

			diskTotal := 1e12 * (1 + phase)
			emit(with("demo_disk_total_bytes"), diskTotal)
			emit(with("demo_disk_usage_bytes"), math.Round(diskTotal*(0.3+0.0001*secs/60)))

			// Batch job every minute; success 90% of the time.
			if int(secs)%60 < 5 {
				st.batchDuration = round3(20 + 10*rng.Float64())
				st.lastBatch = float64(t / 1000)
				st.batchBytes = float64(1e6 + 1e5 - rng.Intn(1e5))
				if rng.Float64() < 0.9 {
					st.lastSuccess = st.lastBatch
				}
			}
			if st.lastBatch > 0 {
				emit(with("demo_batch_last_run_timestamp_seconds"), st.lastBatch)
				emit(with("demo_batch_last_run_duration_seconds"), st.batchDuration)
				emit(with("demo_batch_last_run_processed_bytes"), st.batchBytes)
			}
			if st.lastSuccess > 0 {
				emit(with("demo_batch_last_success_timestamp_seconds"), st.lastSuccess)
			}

			emit(with("demo_is_holiday"), 0)
			st.shipped += float64(rng.Intn(5))
			emit(with("demo_items_shipped_total"), st.shipped)

			inProgress := 0.0
			for _, r := range routes {
				n := 20 + rng.Intn(20)
				outage := time.Duration(secs*float64(time.Second))%(10*r.outage) < r.outage
				for k := 0; k < n; k++ {
					lat := r.latency * (1 + rng.NormFloat64()/10)
					status := "200"
					if outage {
						lat *= 5
					}
					if rng.Float64() < r.errorRatio*boolf(outage)*10+r.errorRatio {
						status = "500"
					}
					key := r.method + " " + r.path + " " + status
					h := st.hist[key]
					if h == nil {
						h = make([]float64, len(buckets)+1)
						st.hist[key] = h
					}
					for bi, ub := range buckets {
						if lat <= ub {
							h[bi]++
						}
					}
					h[len(buckets)]++
					st.sum[key] += lat
					st.count[key]++
				}
				inProgress += float64(rng.Intn(3))
			}
			keys := make([]string, 0, len(st.hist))
			for k := range st.hist {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				var method, path, status string
				splitKey(key, &method, &path, &status)
				lbl := []string{"method", method, "path", path, "status", status}
				h := st.hist[key]
				for bi, ub := range buckets {
					emit(with("demo_api_request_duration_seconds_bucket", append(lbl, "le", formatLe(ub))...), h[bi])
				}
				emit(with("demo_api_request_duration_seconds_bucket", append(lbl, "le", "+Inf")...), h[len(buckets)])
				emit(with("demo_api_request_duration_seconds_sum", lbl...), round3(st.sum[key]))
				emit(with("demo_api_request_duration_seconds_count", lbl...), st.count[key])
			}
			emit(with("demo_api_http_requests_in_progress"), inProgress)

			// Present only in even minutes.
			if int(elapsed.Minutes())%2 == 0 {
				emit(with("demo_intermittent_metric"), 1)
			}

			for k, l := range exposed {
				if _, ok := cur[k]; !ok {
					b.add(l, t, math.Float64frombits(value.StaleNaN))
				}
			}
			exposed = cur
		}
	}

	d := &Dataset{Start: start, End: end}
	for _, k := range b.order {
		d.Series = append(d.Series, *b.series[k])
	}
	return d
}

func splitKey(key string, method, path, status *string) {
	var parts [3]string
	j := 0
	for _, r := range key {
		if r == ' ' {
			j++
			continue
		}
		parts[j] += string(r)
	}
	*method, *path, *status = parts[0], parts[1], parts[2]
}

func formatLe(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
