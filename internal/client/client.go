// Package client talks to a Prometheus-compatible HTTP endpoint: remote write
// for ingestion and the /api/v1/query{,_range} API for evaluation. The same
// client is used for GreptimeDB and for the in-process Prometheus reference, so
// encoding and decoding cannot introduce a difference between the two.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
)

// Series is one input series. Floats and Histograms are both sorted by time.
type Series struct {
	Labels     labels.Labels
	Floats     []promql.FPoint
	Histograms []promql.HPoint
}

// Result is a decoded query response.
type Result struct {
	Value    parser.Value
	Warnings []string
	Infos    []string
}

// Empty reports whether the result is an empty vector or matrix.
func (r *Result) Empty() bool {
	switch v := r.Value.(type) {
	case promql.Vector:
		return len(v) == 0
	case promql.Matrix:
		return len(v) == 0
	}
	return false
}

// QueryError is an error reported by the endpoint through the API error
// envelope, i.e. the query was rejected or failed to evaluate. Transport
// failures are returned as plain errors.
type QueryError struct {
	Type string
	Msg  string
}

func (e *QueryError) Error() string { return e.Msg }

// Endpoint is a Prometheus-compatible HTTP API.
type Endpoint struct {
	WriteURL string // e.g. http://127.0.0.1:4000/v1/prometheus/write
	QueryURL string // e.g. http://127.0.0.1:4000/v1/prometheus/api/v1
	// Params go into the URL query string of every request, e.g. the
	// GreptimeDB database, which it reads only from there.
	Params url.Values
	HTTP   *http.Client
	// SendHistograms controls whether native histogram samples are written.
	SendHistograms bool
}

const maxSamplesPerRequest = 5000

// Write remote-writes the series, splitting into bounded requests.
// Samples of one series are always sent in time order.
func (e *Endpoint) Write(ctx context.Context, series []Series) error {
	var batch []prompb.TimeSeries
	n := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := e.writeOnce(ctx, batch)
		batch, n = nil, 0
		return err
	}
	for _, s := range series {
		lbls := prompb.FromLabels(dropEmpty(s.Labels), nil)
		for i := 0; i < len(s.Floats); i += maxSamplesPerRequest {
			end := min(i+maxSamplesPerRequest, len(s.Floats))
			ts := prompb.TimeSeries{Labels: lbls}
			for _, p := range s.Floats[i:end] {
				ts.Samples = append(ts.Samples, prompb.Sample{Timestamp: p.T, Value: p.F})
			}
			batch = append(batch, ts)
			n += end - i
			if n >= maxSamplesPerRequest {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if e.SendHistograms && len(s.Histograms) > 0 {
			ts := prompb.TimeSeries{Labels: lbls}
			for _, p := range s.Histograms {
				ts.Histograms = append(ts.Histograms, prompb.FromFloatHistogram(p.T, p.H))
			}
			batch = append(batch, ts)
			n += len(s.Histograms)
		}
		if n >= maxSamplesPerRequest {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// dropEmpty removes empty-valued labels: remote write senders never emit
// them and Prometheus treats them as absent.
func dropEmpty(l labels.Labels) labels.Labels {
	b := labels.NewBuilder(l)
	l.Range(func(lbl labels.Label) {
		if lbl.Value == "" {
			b.Del(lbl.Name)
		}
	})
	return b.Labels()
}

func (e *Endpoint) writeOnce(ctx context.Context, ts []prompb.TimeSeries) error {
	req := &prompb.WriteRequest{Timeseries: ts}
	raw, err := req.Marshal()
	if err != nil {
		return err
	}
	u := e.url(e.WriteURL)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(snappy.Encode(nil, raw)))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/x-protobuf")
	hreq.Header.Set("Content-Encoding", "snappy")
	hreq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := e.HTTP.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote write to %s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// FormatTime renders a millisecond timestamp the way Prometheus clients do.
func FormatTime(ms int64) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64)
}

// Instant runs an instant query at ts.
func (e *Endpoint) Instant(ctx context.Context, q string, ts time.Time) (*Result, error) {
	v := url.Values{"query": {q}, "time": {FormatTime(ts.UnixMilli())}}
	return e.query(ctx, "/query", v)
}

// Range runs a range query.
func (e *Endpoint) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) (*Result, error) {
	v := url.Values{
		"query": {q},
		"start": {FormatTime(start.UnixMilli())},
		"end":   {FormatTime(end.UnixMilli())},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return e.query(ctx, "/query_range", v)
}

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
	Infos     []string        `json:"infos"`
}

type apiData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (e *Endpoint) url(base string) string {
	if len(e.Params) == 0 {
		return base
	}
	return base + "?" + e.Params.Encode()
}

// query POSTs the form like Grafana does.
func (e *Endpoint) query(ctx context.Context, path string, v url.Values) (*Result, error) {
	u := e.url(e.QueryURL + path)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.HTTP.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("HTTP %s, undecodable body %q: %w", resp.Status, truncate(string(body), 300), err)
	}
	if ar.Status != "success" {
		if ar.Error == "" {
			return nil, fmt.Errorf("HTTP %s, status %q without error message: %s", resp.Status, ar.Status, truncate(string(body), 300))
		}
		return nil, &QueryError{Type: ar.ErrorType, Msg: ar.Error}
	}
	var d apiData
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		return nil, fmt.Errorf("decode data: %w", err)
	}
	val, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	return &Result{Value: val, Warnings: ar.Warnings, Infos: ar.Infos}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type apiSeries struct {
	Metric     map[string]string   `json:"metric"`
	Value      []json.RawMessage   `json:"value"`
	Values     [][]json.RawMessage `json:"values"`
	Histogram  []json.RawMessage   `json:"histogram"`
	Histograms [][]json.RawMessage `json:"histograms"`
}

func decodeValue(d apiData) (parser.Value, error) {
	switch d.ResultType {
	case "scalar", "string":
		var pair []json.RawMessage
		if err := json.Unmarshal(d.Result, &pair); err != nil {
			return nil, fmt.Errorf("decode %s: %w", d.ResultType, err)
		}
		t, s, err := decodePair(pair)
		if err != nil {
			return nil, err
		}
		if d.ResultType == "string" {
			return promql.String{T: t, V: s}, nil
		}
		f, err := parseFloat(s)
		return promql.Scalar{T: t, V: f}, err
	case "vector":
		var raw []apiSeries
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode vector: %w", err)
		}
		vec := make(promql.Vector, 0, len(raw))
		for _, r := range raw {
			s := promql.Sample{Metric: labels.FromMap(r.Metric)}
			switch {
			case r.Value != nil:
				t, v, err := decodePair(r.Value)
				if err != nil {
					return nil, err
				}
				s.T = t
				if s.F, err = parseFloat(v); err != nil {
					return nil, err
				}
			case r.Histogram != nil:
				t, h, err := decodeHistogram(r.Histogram)
				if err != nil {
					return nil, err
				}
				s.T, s.H = t, h
			default:
				return nil, errors.New("vector sample without value")
			}
			vec = append(vec, s)
		}
		return vec, nil
	case "matrix":
		var raw []apiSeries
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode matrix: %w", err)
		}
		mat := make(promql.Matrix, 0, len(raw))
		for _, r := range raw {
			s := promql.Series{Metric: labels.FromMap(r.Metric)}
			for _, pair := range r.Values {
				t, v, err := decodePair(pair)
				if err != nil {
					return nil, err
				}
				f, err := parseFloat(v)
				if err != nil {
					return nil, err
				}
				s.Floats = append(s.Floats, promql.FPoint{T: t, F: f})
			}
			for _, pair := range r.Histograms {
				t, h, err := decodeHistogram(pair)
				if err != nil {
					return nil, err
				}
				s.Histograms = append(s.Histograms, promql.HPoint{T: t, H: h})
			}
			mat = append(mat, s)
		}
		return mat, nil
	default:
		return nil, fmt.Errorf("unknown resultType %q", d.ResultType)
	}
}

// decodePair decodes [<unix seconds>, "<string>"] keeping millisecond
// precision exact by parsing the timestamp text instead of going through float.
func decodePair(pair []json.RawMessage) (int64, string, error) {
	if len(pair) != 2 {
		return 0, "", fmt.Errorf("expected [ts, value], got %d elements", len(pair))
	}
	t, err := parseTimestamp(string(pair[0]))
	if err != nil {
		return 0, "", err
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, "", fmt.Errorf("value %s is not a string: %w", pair[1], err)
	}
	return t, s, nil
}

func parseTimestamp(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad timestamp %q: %w", s, err)
	}
	frac = (frac + "000")[:3]
	ms, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad timestamp %q: %w", s, err)
	}
	t := sec*1000 + ms
	if neg {
		t = -t
	}
	return t, nil
}

func parseFloat(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

type apiHistogram struct {
	Count   string              `json:"count"`
	Sum     string              `json:"sum"`
	Buckets [][]json.RawMessage `json:"buckets"`
}

// decodeHistogram rebuilds a custom-bucket float histogram from the API form.
// The API does not carry the schema, so this is only good enough for
// reporting; native histogram results are not part of the compatibility rate.
func decodeHistogram(pair []json.RawMessage) (int64, *histogram.FloatHistogram, error) {
	if len(pair) != 2 {
		return 0, nil, fmt.Errorf("expected [ts, histogram], got %d elements", len(pair))
	}
	t, err := parseTimestamp(string(pair[0]))
	if err != nil {
		return 0, nil, err
	}
	var ah apiHistogram
	if err := json.Unmarshal(pair[1], &ah); err != nil {
		return 0, nil, err
	}
	h := &histogram.FloatHistogram{Schema: histogram.CustomBucketsSchema}
	if h.Count, err = parseFloat(ah.Count); err != nil {
		return 0, nil, err
	}
	if h.Sum, err = parseFloat(ah.Sum); err != nil {
		return 0, nil, err
	}
	type bucket struct{ upper, count float64 }
	var bs []bucket
	for _, b := range ah.Buckets {
		if len(b) != 4 {
			continue
		}
		var up, cnt string
		_ = json.Unmarshal(b[2], &up)
		_ = json.Unmarshal(b[3], &cnt)
		u, _ := parseFloat(up)
		c, _ := parseFloat(cnt)
		bs = append(bs, bucket{u, c})
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].upper < bs[j].upper })
	for _, b := range bs {
		if !math.IsInf(b.upper, 1) {
			h.CustomValues = append(h.CustomValues, b.upper)
		}
		h.PositiveBuckets = append(h.PositiveBuckets, b.count)
	}
	if len(bs) > 0 {
		h.PositiveSpans = []histogram.Span{{Offset: 0, Length: uint32(len(bs))}}
	}
	return t, h, nil
}
