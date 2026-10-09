// Package promref is the reference side of the oracle: an in-process server
// that exposes the Prometheus remote write receiver and query API on top of the
// upstream Prometheus TSDB and PromQL engine, with one TSDB per ?db= value.
// Responses are encoded with Prometheus's own API JSON codec.
package promref

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
	apiv1 "github.com/prometheus/prometheus/web/api/v1"
)

// Profile selects the engine configuration.
type Profile string

const (
	// ProfileTest matches promqltest.NewTestEngine, the configuration the
	// upstream corpus expectations were written against.
	ProfileTest Profile = "test"
	// ProfileServer matches a Prometheus 3 server started without feature
	// flags: no experimental functions, no delayed __name__ removal.
	ProfileServer Profile = "server"
)

type Server struct {
	engine *promql.Engine
	parser parser.Parser
	lis    net.Listener
	srv    *http.Server

	mu  sync.Mutex
	dbs map[string]*db
}

type db struct {
	st    *teststorage.TestStorage
	write http.Handler
}

// Start listens on 127.0.0.1 with a random port.
func Start(p Profile) (*Server, error) {
	var parserOpts parser.Options
	opts := promql.EngineOpts{
		MaxSamples:               50_000_000,
		Timeout:                  100 * time.Second,
		NoStepSubqueryIntervalFn: func(int64) int64 { return time.Minute.Milliseconds() },
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		LookbackDelta:            5 * time.Minute,
	}
	switch p {
	case ProfileTest:
		parserOpts = parser.Options{
			EnableExperimentalFunctions:  true,
			ExperimentalDurationExpr:     true,
			EnableExtendedRangeSelectors: true,
			EnableBinopFillModifiers:     true,
		}
		opts.MaxSamples = 10000
		opts.EnableDelayedNameRemoval = true
		opts.UseStartTimestamps = true
	case ProfileServer:
	default:
		return nil, fmt.Errorf("unknown profile %q", p)
	}
	opts.Parser = parser.NewParser(parserOpts)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{engine: promql.NewEngine(opts), parser: opts.Parser, lis: lis, dbs: map[string]*db{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/write", s.handleWrite)
	mux.HandleFunc("/api/v1/query", s.handleQuery)
	mux.HandleFunc("/api/v1/query_range", s.handleQuery)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(lis) }()
	return s, nil
}

func (s *Server) Addr() string { return "http://" + s.lis.Addr().String() }

func (s *Server) WriteURL() string { return s.Addr() + "/write" }
func (s *Server) QueryURL() string { return s.Addr() + "/api/v1" }

// CreateDB creates an empty dataset.
func (s *Server) CreateDB(name string) error {
	st, err := teststorage.NewWithError(func(o *tsdb.Options) {
		// Same options as promqltest.RunBuiltinTests.
		o.EnableSTStorage = true
		o.FloatChunkEncoding = chunkenc.EncXOR2
		o.EnableHistogramSTEncoding = true
	})
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h := remote.NewWriteHandler(logger, prometheus.NewRegistry(), st, remoteapi.MessageTypes{remoteapi.WriteV1MessageType}, false, false, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.dbs[name]; ok {
		_ = old.st.Close()
	}
	s.dbs[name] = &db{st: st, write: h}
	return nil
}

func (s *Server) DropDB(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dbs[name]; ok {
		_ = d.st.Close()
		delete(s.dbs, name)
	}
}

func (s *Server) Close() {
	_ = s.srv.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.dbs {
		_ = d.st.Close()
	}
	_ = s.engine.Close()
}

func (s *Server) lookup(name string) (*db, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dbs[name]
	return d, ok
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(r.URL.Query().Get("db"))
	if !ok {
		http.Error(w, "unknown db", http.StatusNotFound)
		return
	}
	d.write.ServeHTTP(w, r)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, "bad_data", err)
		return
	}
	d, ok := s.lookup(r.URL.Query().Get("db"))
	if !ok {
		writeError(w, "bad_data", errors.New("unknown db"))
		return
	}
	qs := r.Form.Get("query")
	ctx := r.Context()
	var (
		q   promql.Query
		err error
	)
	if r.URL.Path == "/api/v1/query" {
		var ts time.Time
		if ts, err = parseTime(r.Form.Get("time")); err != nil {
			writeError(w, "bad_data", err)
			return
		}
		q, err = s.engine.NewInstantQuery(ctx, d.st, nil, qs, ts)
	} else {
		var start, end time.Time
		var step time.Duration
		if start, err = parseTime(r.Form.Get("start")); err == nil {
			if end, err = parseTime(r.Form.Get("end")); err == nil {
				step, err = parseDuration(r.Form.Get("step"))
			}
		}
		if err != nil {
			writeError(w, "bad_data", err)
			return
		}
		q, err = s.engine.NewRangeQuery(ctx, d.st, nil, qs, start, end, step)
	}
	if err != nil {
		writeError(w, "bad_data", err)
		return
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		writeError(w, "execution", res.Err)
		return
	}
	warnings, infos := res.Warnings.AsStrings(qs, 0, 0)
	resp := &apiv1.Response{
		Status:   "success",
		Data:     &apiv1.QueryData{ResultType: res.Value.Type(), Result: res.Value},
		Warnings: warnings,
		Infos:    infos,
	}
	b, err := apiv1.JSONCodec{}.Encode(resp)
	if err != nil {
		writeError(w, "internal", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, typ string, err error) {
	b, _ := apiv1.JSONCodec{}.Encode(&apiv1.Response{Status: "error", ErrorType: typ, Error: err.Error()})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write(b)
}

// parseTime and parseDuration follow web/api/v1 for the numeric forms the
// client sends.
func parseTime(s string) (time.Time, error) {
	t, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q to a valid timestamp", s)
	}
	sec, ns := math.Modf(t)
	ns = math.Round(ns*1000) / 1000
	return time.Unix(int64(sec), int64(ns*float64(time.Second))).UTC(), nil
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := strconv.ParseFloat(s, 64); err == nil {
		ts := d * float64(time.Second)
		if ts > float64(math.MaxInt64) || ts < float64(math.MinInt64) {
			return 0, fmt.Errorf("cannot parse %q to a valid duration. It overflows int64", s)
		}
		return time.Duration(ts), nil
	}
	d, err := model.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q to a valid duration", s)
	}
	return time.Duration(d), nil
}
