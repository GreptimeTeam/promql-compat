// Command promql-compat measures PromQL compatibility of GreptimeDB or VictoriaMetrics against
// the Prometheus engine and writes a report.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/GreptimeTeam/promql-compat/internal/corpus"
	"github.com/GreptimeTeam/promql-compat/internal/par"
	"github.com/GreptimeTeam/promql-compat/internal/promref"
	"github.com/GreptimeTeam/promql-compat/internal/report"
	"github.com/GreptimeTeam/promql-compat/internal/result"
	"github.com/GreptimeTeam/promql-compat/internal/target"
)

func main() {
	var (
		greptimeBin = flag.String("greptime-bin", "", "start this greptime binary as the target (standalone, private data dir)")
		greptimeVer = flag.String("greptime-version", "", "download this GreptimeDB release (e.g. v1.2.1) and start it as the target")
		greptimeURL = flag.String("greptime-url", "", "use an already running GreptimeDB at this HTTP base URL")
		vmBin       = flag.String("vm-bin", "", "start this VictoriaMetrics single-node binary as the target (private data dir)")
		vmVer       = flag.String("vm-version", "", "download this VictoriaMetrics release (e.g. v1.153.0) and start it as the target")
		control     = flag.Bool("control", false, "run against the in-process Prometheus (harness self-check)")
		port        = flag.Int("port", 4600, "first port of a started target (GreptimeDB uses 4 consecutive ports, VictoriaMetrics 1)")
		suites      = flag.String("suites", "corpus,compliance,fuzz", "comma-separated suites to run")
		filter      = flag.String("files", "", "corpus: only files whose name contains this substring")
		outDir      = flag.String("out", "", "output directory (default out/<timestamp>)")
		label       = flag.String("label", "", "free-form label for the report (e.g. branch name)")
		keepData    = flag.Bool("keep-data", false, "keep created databases and the data directory of a started target")
		fuzzCount   = flag.Int("fuzz-count", 2000, "fuzz: number of generated queries")
		fuzzSeed    = flag.Int64("fuzz-seed", 1, "fuzz: random seed")
		minimize    = flag.Bool("shrink", true, "fuzz: shrink failing queries to minimal repros (slow; not needed for rates)")
		workers     = flag.Int("parallel", 8, "concurrent cases (corpus: concurrent files)")
		probeQuery  = flag.String("probe", "", "print both results of these queries (separated by ';;') on the demo dataset and exit")
		probeTime   = flag.String("probe-time", "52m30s", "probe: offset from the dataset start, or from..to/step")
		probeData   = flag.String("probe-data", "", "probe: promqltest file whose load commands replace the demo dataset (times then count from 0)")
	)
	rerender := flag.String("rerender", "", "rewrite REPORT.md and REPORT.html next to this results.json and exit")
	compareA := flag.String("compare", "", "base results.json; with -against, print flipped cases and exit (status 1 on regressions)")
	compareB := flag.String("against", "", "new results.json for -compare")
	flag.Parse()
	if *compareA != "" {
		regressed, err := compareRuns(*compareA, *compareB)
		if err != nil {
			log.Fatal(err)
		}
		if regressed > 0 {
			os.Exit(1)
		}
		return
	}
	if *rerender != "" {
		raw, err := os.ReadFile(*rerender)
		if err != nil {
			log.Fatal(err)
		}
		var rep report.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			log.Fatal(err)
		}
		if err := writeReports(filepath.Dir(*rerender), &rep); err != nil {
			log.Fatal(err)
		}
		fmt.Print(rep.Summary())
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *outDir == "" {
		*outDir = filepath.Join("out", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	runID := "pc" + time.Now().Format("150405")

	var suiteNames []string
	for _, s := range strings.Split(*suites, ",") {
		if s = strings.TrimSpace(s); s != "" {
			suiteNames = append(suiteNames, s)
		}
	}

	sources := 0
	for _, set := range []bool{*control, *greptimeBin != "", *greptimeVer != "", *greptimeURL != "", *vmBin != "", *vmVer != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		log.Fatal("exactly one of --control, --greptime-bin, --greptime-version, --greptime-url, --vm-bin, --vm-version is required")
	}
	if *vmBin != "" || *vmVer != "" {
		// VictoriaMetrics rejects samples before 1970-01-02; the corpus loads
		// its data from the epoch.
		if *probeData != "" || (*probeQuery == "" && slices.Contains(suiteNames, "corpus")) {
			log.Fatal("VictoriaMetrics accepts samples from 1970-01-02 only; the corpus suite and --probe-data are not supported")
		}
	}
	fetch := func(product, tag string) string {
		log.Printf("fetching %s %s", product, tag)
		bin, err := target.Fetch(ctx, product, tag)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("using %s", bin)
		return bin
	}
	if *greptimeVer != "" {
		*greptimeBin = fetch("greptime", *greptimeVer)
	}
	if *vmVer != "" {
		*vmBin = fetch("victoriametrics", *vmVer)
	}
	started := func(proc *target.Process) {
		// log.Fatal skips deferred calls; never leave the server running.
		stopOnFatal = proc.Stop
		log.Printf("started pid %d at %s (log %s)", proc.Cmd.Process.Pid, proc.HTTP, proc.LogPath)
	}

	var (
		tgt     target.Target
		version string
		// In control mode the diff suites compare the server profile with
		// itself; the corpus needs the test profile its expectations assume.
		diffTgt target.Target
	)
	switch {
	case *control:
		p, err := target.NewPromref(promref.ProfileTest)
		if err != nil {
			log.Fatal(err)
		}
		defer p.S.Close()
		ps, err := target.NewPromref(promref.ProfileServer)
		if err != nil {
			log.Fatal(err)
		}
		defer ps.S.Close()
		tgt, diffTgt, version = p, ps, "in-process Prometheus engine (control)"
	case *greptimeBin != "":
		work, _ := filepath.Abs(*outDir)
		proc, err := target.LaunchGreptime(ctx, *greptimeBin, *port, work, *keepData)
		if err != nil {
			log.Fatal(err)
		}
		defer proc.Stop()
		started(proc)
		g := target.NewGreptime(proc.HTTP)
		tgt = g
		version, _ = g.Version(ctx)
	case *greptimeURL != "":
		g := target.NewGreptime(*greptimeURL)
		tgt = g
		version, _ = g.Version(ctx)
	case *vmBin != "":
		v, err := target.VMVersion(*vmBin)
		if err != nil {
			log.Fatalf("%s --version: %v", *vmBin, err)
		}
		work, _ := filepath.Abs(*outDir)
		proc, err := target.LaunchVM(ctx, *vmBin, *port, work, *keepData)
		if err != nil {
			log.Fatal(err)
		}
		defer proc.Stop()
		started(proc)
		tgt = target.NewVM(proc.HTTP)
		version = v + " " + strings.Join(target.VMFlags, " ")
	}

	if diffTgt == nil {
		diffTgt = tgt
	}
	if *probeQuery != "" {
		if err := probe(ctx, diffTgt, runID, strings.Split(*probeQuery, ";;"), *probeTime, *probeData); err != nil {
			fatal(err)
		}
		return
	}
	rep := &report.Report{
		Target:  tgt.Name(),
		Version: strings.TrimSpace(version),
		Label:   *label,
		Config:  runConfig(suiteNames, *filter, *fuzzCount, *fuzzSeed),
		Started: time.Now(),
	}
	var env *diffEnv
	for _, s := range suiteNames {
		var cases []result.Case
		var err error
		start := time.Now()
		switch s {
		case "corpus":
			cases, err = runCorpus(ctx, tgt, runID, *filter, *workers, *keepData)
		case "compliance", "fuzz":
			if env == nil {
				if env, err = newDiffEnv(ctx, diffTgt, runID, *keepData); err != nil {
					fatal(fmt.Sprintf("load demo dataset: %v", err))
				}
				defer env.close()
			}
			cases, err = env.run(ctx, s, *fuzzCount, *fuzzSeed, *workers, *minimize)
		default:
			fatal(fmt.Sprintf("unknown suite %q", s))
		}
		if err != nil {
			fatal(fmt.Sprintf("suite %s: %v", s, err))
		}
		log.Printf("suite %s: %d cases in %s", s, len(cases), time.Since(start).Round(time.Second))
		rep.Cases = append(rep.Cases, cases...)
	}
	rep.Finished = time.Now()

	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "results.json"), raw, 0o644); err != nil {
		fatal(err)
	}
	if err := writeReports(*outDir, rep); err != nil {
		fatal(err)
	}
	fmt.Print(rep.Summary())
	fmt.Printf("\nreport: %s\n        %s\n", filepath.Join(*outDir, "REPORT.md"), filepath.Join(*outDir, "REPORT.html"))
	if *control {
		for i := range rep.Cases {
			if c := &rep.Cases[i]; !c.Excluded() && !c.Status.Passed() {
				fatal("control: in-scope cases failed; the harness disagrees with itself")
			}
		}
	}
}

func writeReports(dir string, rep *report.Report) error {
	if err := os.WriteFile(filepath.Join(dir, "REPORT.md"), []byte(rep.Markdown()), 0o644); err != nil {
		return err
	}
	page, err := rep.HTML()
	if err != nil {
		return fmt.Errorf("render REPORT.html: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "REPORT.html"), []byte(page), 0o644)
}

// runConfig records what determines the cases, so a run can be reproduced
// and two runs checked for comparability.
func runConfig(suites []string, files string, fuzzCount int, fuzzSeed int64) report.Config {
	cfg := report.Config{Args: os.Args, Suites: suites, Files: files, FuzzCount: fuzzCount, FuzzSeed: fuzzSeed}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return cfg
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/prometheus/prometheus" {
			cfg.Reference = d.Version
		}
	}
	dirty := false
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			cfg.Harness = kv.Value
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	if cfg.Harness != "" && dirty {
		cfg.Harness += "+dirty"
	}
	return cfg
}

// stopOnFatal stops the server started by the harness before a fatal exit.
var stopOnFatal = func() {}

func fatal(v ...any) {
	stopOnFatal()
	log.Fatal(v...)
}

func runCorpus(ctx context.Context, tgt target.Target, runID, filter string, workers int, keep bool) ([]result.Case, error) {
	all, err := corpus.Files()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range all {
		if filter == "" || strings.Contains(f, filter) {
			files = append(files, f)
		}
	}
	r := &corpus.Runner{Target: tgt, RunID: runID, KeepData: keep}
	perFile := make([][]result.Case, len(files))
	errs := make([]error, len(files))
	par.Do(len(files), workers, func(i int) {
		content, err := corpus.ReadFile(files[i])
		if err == nil {
			perFile[i], err = r.RunFile(ctx, files[i], content)
		}
		errs[i] = err
	})
	var cases []result.Case
	for i := range files {
		if errs[i] != nil {
			return nil, errs[i]
		}
		cases = append(cases, perFile[i]...)
	}
	return cases, ctx.Err()
}
