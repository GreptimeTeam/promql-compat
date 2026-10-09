package diff

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"

	"github.com/GreptimeTeam/promql-compat/internal/client"
	"github.com/GreptimeTeam/promql-compat/internal/promref"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

type deadTarget struct{ ep *client.Endpoint }

func (d deadTarget) Name() string                           { return "dead" }
func (d deadTarget) Endpoint(string) *client.Endpoint       { return d.ep }
func (d deadTarget) Ready(context.Context, string) error    { return nil }
func (d deadTarget) CreateDB(context.Context, string) error { return nil }
func (d deadTarget) DropDB(context.Context, string) error   { return nil }

// A failed second reference request is not evidence of nondeterminism; it
// must not exclude the case and hide the target's wrong answer.
func TestRecheckFailureIsNotNondeterministic(t *testing.T) {
	ctx := context.Background()
	newRef := func(v float64) *target.Promref {
		p, err := target.NewPromref(promref.ProfileServer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.S.Close)
		if err := p.CreateDB(ctx, "db"); err != nil {
			t.Fatal(err)
		}
		s := client.Series{Labels: labels.FromStrings("__name__", "m"), Floats: []promql.FPoint{{T: 0, F: v}}}
		if err := p.Endpoint("db").Write(ctx, []client.Series{s}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dead := deadTarget{&client.Endpoint{QueryURL: "http://127.0.0.1:1/api/v1", HTTP: &http.Client{Timeout: time.Second}}}
	r := &Runner{Ref: newRef(1), Target: newRef(2), Ref2: dead}
	ch := r.Check(ctx, "db", "m", Query{Start: time.Unix(0, 0)})
	if ch.Nondeterministic || ch.Status.Passed() {
		t.Fatalf("got status %s, nondeterministic %v", ch.Status, ch.Nondeterministic)
	}
}
