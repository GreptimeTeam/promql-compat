// Package target wraps a system under test (or the reference) as a set of
// isolated datasets reachable through the Prometheus HTTP API.
package target

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GreptimeTeam/promql-compat/internal/client"
	"github.com/GreptimeTeam/promql-compat/internal/promref"
)

type Target interface {
	Name() string
	// Endpoint addresses dataset db.
	Endpoint(db string) *client.Endpoint
	CreateDB(ctx context.Context, name string) error
	DropDB(ctx context.Context, name string) error
	// Ready makes everything written to db visible to queries.
	Ready(ctx context.Context, db string) error
}

// dbEndpoint addresses a dataset selected by the ?db= query parameter.
func dbEndpoint(base client.Endpoint, db string) *client.Endpoint {
	base.Params = url.Values{"db": {db}}
	return &base
}

func httpClient() *http.Client { return &http.Client{Timeout: 120 * time.Second} }

// Greptime is a running GreptimeDB HTTP server.
type Greptime struct {
	BaseURL string // e.g. http://127.0.0.1:4600
	ep      client.Endpoint
}

func NewGreptime(baseURL string) *Greptime {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Greptime{BaseURL: baseURL, ep: client.Endpoint{
		WriteURL: baseURL + "/v1/prometheus/write",
		QueryURL: baseURL + "/v1/prometheus/api/v1",
		HTTP:     httpClient(),
	}}
}

func (g *Greptime) Name() string                        { return "greptimedb" }
func (g *Greptime) Endpoint(db string) *client.Endpoint { return dbEndpoint(g.ep, db) }
func (g *Greptime) Ready(context.Context, string) error { return nil }
func (g *Greptime) CreateDB(ctx context.Context, name string) error {
	return g.SQL(ctx, "CREATE DATABASE IF NOT EXISTS "+name)
}
func (g *Greptime) DropDB(ctx context.Context, name string) error {
	return g.SQL(ctx, "DROP DATABASE IF EXISTS "+name)
}

// SQL runs a statement through the HTTP SQL API.
func (g *Greptime) SQL(ctx context.Context, sql string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/sql",
		strings.NewReader(url.Values{"sql": {sql}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.ep.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", sql, resp.Status, body)
	}
	return nil
}

// Version returns the server's build info as reported by the Prometheus API.
func (g *Greptime) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/v1/prometheus/api/v1/status/buildinfo", nil)
	if err != nil {
		return "", err
	}
	resp, err := g.ep.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// Promref is the in-process Prometheus reference.
type Promref struct {
	S  *promref.Server
	ep client.Endpoint
}

func NewPromref(p promref.Profile) (*Promref, error) {
	s, err := promref.Start(p)
	if err != nil {
		return nil, err
	}
	return &Promref{S: s, ep: client.Endpoint{
		WriteURL:       s.WriteURL(),
		QueryURL:       s.QueryURL(),
		HTTP:           httpClient(),
		SendHistograms: true,
	}}, nil
}

func (p *Promref) Name() string                        { return "prometheus" }
func (p *Promref) Endpoint(db string) *client.Endpoint { return dbEndpoint(p.ep, db) }
func (p *Promref) Ready(context.Context, string) error { return nil }
func (p *Promref) CreateDB(_ context.Context, name string) error {
	return p.S.CreateDB(name)
}
func (p *Promref) DropDB(_ context.Context, name string) error {
	p.S.DropDB(name)
	return nil
}
