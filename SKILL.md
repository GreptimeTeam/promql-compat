---
name: promql-compat
description: Run the PromQL compatibility harness against GreptimeDB or VictoriaMetrics, compare two runs, and investigate a failing query. Use when asked to measure PromQL compatibility, check a GreptimeDB branch or release for PromQL regressions, compare compatibility reports, or reproduce a PromQL difference against Prometheus.
---

# Running promql-compat

The harness measures one target per run against the embedded Prometheus engine and writes
`REPORT.md` and `results.json` to `--out` (default `out/<timestamp>/`, gitignored).

## Build and self-check

```bash
go build -o bin/promql-compat ./cmd/promql-compat
./bin/promql-compat --control --out /tmp/pc-control
```

`--control` runs the reference against itself and exits 1 if any in-scope case fails. Run it
after changing harness code; a failure there is a harness bug, not a target result. Unit tests
(`go test ./...`) cover the comparator; a broken comparator can also show up in control as most
cases excluded as `nondeterministic`, so check the excluded count, not only the rate.

## Measure a target

Exactly one target flag is required.

| Target | Flag | Suites |
|---|---|---|
| GreptimeDB release | `--greptime-version v1.2.1` | all |
| GreptimeDB local build | `--greptime-bin /path/to/greptime` | all |
| GreptimeDB already running | `--greptime-url http://127.0.0.1:4000` | all |
| VictoriaMetrics release | `--vm-version v1.153.0` | `--suites compliance,fuzz` (required) |
| VictoriaMetrics local binary | `--vm-bin /path/to/victoria-metrics-prod` | `--suites compliance,fuzz` (required) |

- Started targets use ports from `--port` (default 4600; GreptimeDB takes 4 consecutive ports,
  VictoriaMetrics 1). The run aborts if a port is busy; pick another `--port` when running
  several targets in parallel.
- Releases are downloaded once, checked against the published sha256, and cached under the user
  cache directory.
- Fuzz shrinking dominates the run time. `--shrink=false` skips it; it does not change any rate,
  only drops the `minimized` queries.
- Use `--label <branch or version>` so `-compare` output is readable.

## Read the result

The console summary and `REPORT.md` give per suite:

- **strict**: the compatibility rate. Excluded cases (native histograms, start timestamps,
  experimental features, delayed `__name__` removal, nondeterministic) are not in it.
- **primary / values-only** views: the rate when ignoring corpus `@`/range-mode variants, or
  differences only in `__name__`. Use them to size a root cause, not as the headline number.
- **passed with data**: passes where a query returned a non-empty result. Empty-vs-empty and
  error-vs-error passes do not exercise evaluation.
- Failure causes, top failure signatures and the coverage table point to what to investigate.

Report the strict rate with the target version, harness revision and suites from the report
header. Do not quote a rate without the excluded count.

## Compare two runs

```bash
./bin/promql-compat -compare out/base/results.json -against out/new/results.json
```

Exits 1 if an in-scope case regressed. `CONFIG DIFFERS` lines mean the runs are not directly
comparable (different harness revision, fuzz seed or count, suites). GreptimeDB corpus failure
details vary between identical runs; compare statuses, not detail text.

## Investigate a failing query

Take the query and time from the failure in `REPORT.md` (fuzz failures also have a
`minimized` query) and print both answers:

```bash
# Demo dataset (compliance, fuzz): time is an offset from the dataset start, or from..to/step.
./bin/promql-compat --greptime-version v1.2.1 --probe 'rate(demo_cpu_usage_seconds_total[1m])' --probe-time 52m30s
./bin/promql-compat --vm-version v1.153.0 --probe 'timestamp(demo_num_cpus)' --probe-time 40m..50m/30s

# Corpus failures: put the case's load blocks (the "repro" field in results.json) into a file;
# times count from 0. Not supported for VictoriaMetrics.
./bin/promql-compat --greptime-version v1.2.1 --probe-data repro.test --probe 'max(m)' --probe-time 0s
```

Separate several queries with `;;`. Before calling a difference a target bug, confirm it on the
smallest query that still differs.

## Pitfalls

- Never commit `out/` or `bin/`.
- VictoriaMetrics cannot run the corpus: it rejects samples before 1970-01-02.
- The diff suites compare floats with a relative tolerance of 1e-10, or 1e-6 when the query uses
  `deriv`, `predict_linear`, `stddev`, `stdvar` or their `_over_time` forms. In such a query a
  difference elsewhere below 1e-6 still passes, and so does a difference diluted by a much larger
  addend. The corpus keeps upstream's 1e-6. Check suspicious passes with `--probe`.
- `--keep-data` keeps the target's data directory under `--out` for inspection after the run.
