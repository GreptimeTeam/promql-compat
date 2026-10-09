package target

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/GreptimeTeam/promql-compat/internal/client"
)

// VM is a VictoriaMetrics single-node owned by the harness. Its HTTP API has
// no dataset isolation, and it keeps every copy of a sample written twice, so
// an instance holds exactly one dataset.
type VM struct {
	BaseURL string
	ep      client.Endpoint
	dataset string
}

func NewVM(baseURL string) *VM {
	baseURL = strings.TrimRight(baseURL, "/")
	return &VM{BaseURL: baseURL, ep: client.Endpoint{
		WriteURL: baseURL + "/api/v1/write",
		QueryURL: baseURL + "/api/v1",
		HTTP:     httpClient(),
	}}
}

func (v *VM) Name() string { return "victoriametrics" }

func (v *VM) Endpoint(string) *client.Endpoint {
	ep := v.ep
	return &ep
}

func (v *VM) CreateDB(_ context.Context, name string) error {
	if v.dataset != "" && v.dataset != name {
		return fmt.Errorf("VictoriaMetrics instance already holds dataset %s; it cannot isolate %s", v.dataset, name)
	}
	v.dataset = name
	return nil
}

// DropDB is a no-op: the instance and its data belong to this run.
func (v *VM) DropDB(context.Context, string) error { return nil }

// Ready flushes in-memory samples; until then they are not searchable.
func (v *VM) Ready(ctx context.Context, _ string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.BaseURL+"/internal/force_flush", nil)
	if err != nil {
		return err
	}
	resp, err := v.ep.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("force_flush: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// VMVersion returns the version printed by the binary. The server's
// /api/v1/status/buildinfo reports a fixed Prometheus version instead.
func VMVersion(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").Output()
	return strings.TrimSpace(string(out)), err
}
