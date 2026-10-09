package target

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process is a server started by the harness.
type Process struct {
	Cmd      *exec.Cmd
	DataDir  string
	HTTP     string
	LogPath  string
	KeepData bool
	exited   chan error
}

func addr(p int) string { return "127.0.0.1:" + strconv.Itoa(p) }

// LaunchGreptime starts `greptime standalone start` on basePort..basePort+3
// with a private data home.
func LaunchGreptime(ctx context.Context, bin string, basePort int, workDir string, keepData bool) (*Process, error) {
	ports := []int{basePort, basePort + 1, basePort + 2, basePort + 3}
	return launch(ctx, "greptime", workDir, ports, keepData, func(dataDir string) *exec.Cmd {
		return exec.Command(bin, "standalone", "start",
			"--http-addr", addr(ports[0]),
			"--grpc-bind-addr", addr(ports[1]),
			"--mysql-addr", addr(ports[2]),
			"--postgres-addr", addr(ports[3]),
			"--data-home", dataDir,
		)
	})
}

// VMFlags are the VictoriaMetrics flags the harness sets besides addresses:
// the demo dataset lies in the past, beyond the default 1 month retention,
// and the rollup result cache would serve results computed before a load.
var VMFlags = []string{"-retentionPeriod=100y", "-search.disableCache"}

// LaunchVM starts a VictoriaMetrics single-node on port with a private
// storage directory.
func LaunchVM(ctx context.Context, bin string, port int, workDir string, keepData bool) (*Process, error) {
	return launch(ctx, "victoriametrics", workDir, []int{port}, keepData, func(dataDir string) *exec.Cmd {
		args := append([]string{"-storageDataPath", dataDir, "-httpListenAddr", addr(port)}, VMFlags...)
		return exec.Command(bin, args...)
	})
}

// launch starts the command built by cmdFor with a fresh data directory and
// waits until GET /health on ports[0] succeeds. It refuses to start if any
// port is taken, so a query can never reach a server the harness does not own.
func launch(ctx context.Context, name, workDir string, ports []int, keepData bool, cmdFor func(dataDir string) *exec.Cmd) (*Process, error) {
	for _, p := range ports {
		l, err := net.Listen("tcp", addr(p))
		if err != nil {
			return nil, fmt.Errorf("port %d is busy; pick another --port: %w", p, err)
		}
		_ = l.Close()
	}
	dataDir, err := os.MkdirTemp(workDir, "promql-compat-"+name+"-")
	if err != nil {
		return nil, err
	}
	logPath := dataDir + ".log"
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := cmdFor(dataDir)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	p := &Process{Cmd: cmd, DataDir: dataDir, HTTP: "http://" + addr(ports[0]), LogPath: logPath, KeepData: keepData, exited: exited}
	deadline := time.Now().Add(120 * time.Second)
	for {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("%s exited during startup (%v); see %s", name, err, logPath)
		default:
		}
		if time.Now().After(deadline) {
			p.Stop()
			return nil, fmt.Errorf("%s did not become healthy in 120s; see %s", name, logPath)
		}
		if healthy(ctx, p.HTTP) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err := verifyOwner(ports[0], cmd.Process.Pid); err != nil {
		p.Stop()
		return nil, err
	}
	return p, nil
}

func healthy(ctx context.Context, base string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// verifyOwner checks that the listener on port belongs to pid (macOS/Linux
// with lsof); skipped when lsof is unavailable.
func verifyOwner(port, pid int) error {
	out, err := exec.Command("lsof", "-nP", "-t", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	for _, f := range strings.Fields(string(out)) {
		if f == strconv.Itoa(pid) {
			return nil
		}
	}
	return fmt.Errorf("listener on port %d is %q, not the started process (pid %d)", port, strings.TrimSpace(string(out)), pid)
}

// Stop terminates the process and, unless KeepData, removes its data
// directory; the log is kept.
func (p *Process) Stop() {
	if p.Cmd.Process != nil {
		_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.exited:
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if !p.KeepData {
		_ = os.RemoveAll(p.DataDir)
	}
}
