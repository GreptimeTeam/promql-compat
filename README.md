# promql-compat

[![CI](https://github.com/GreptimeTeam/promql-compat/actions/workflows/ci.yml/badge.svg)](https://github.com/GreptimeTeam/promql-compat/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/GreptimeTeam/promql-compat)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Measures PromQL compatibility of GreptimeDB and VictoriaMetrics against the Prometheus 3.15.0
engine (Go module `github.com/prometheus/prometheus` v0.315.0, embedded) and writes a report with
compatibility rates. Each run measures one target.

Data reaches both sides through Prometheus remote write. Targets are queried through their
Prometheus HTTP API (GreptimeDB: `/v1/prometheus/api/v1`, VictoriaMetrics: `/api/v1`); the
reference is an in-process server built from the Prometheus remote write receiver, TSDB, PromQL
engine and API JSON codec, exposing the same API. The same HTTP client is used for all of them.

## Suites

| Suite | Oracle | Data | Cases |
|---|---|---|---|
| `corpus` | Expected results in the upstream promqltest files (`promql/promqltest/testdata`) | `load` blocks of each file, one fresh database per load | 2243 eval commands |
| `compliance` | Prometheus engine, server defaults | Deterministic demo-service dataset (3 targets, 5s scrapes with jitter, a target outage with stale markers, a counter reset, a month boundary) | 539 queries from prometheus/compliance `promql-test-queries.yml`, 2 range + 3 instant evaluations each |
| `fuzz` | Prometheus engine, server defaults | Same demo dataset | Generated type-correct queries (default 2000, seed 1); failures are shrunk to minimal repros |

The corpus replays upstream promqltest semantics: each instant eval also runs `@`-modifier
variants and a range-mode self-check. Parsing and result comparison are forked unchanged from
upstream (`internal/corpus/promqltest.go`).

Cases are excluded from the rate when they need native histograms, start timestamps,
experimental PromQL features, or delayed `__name__` removal, or when two reference instances
disagree (tie-breaking in `topk`, float summation order). Excluded cases are reported separately.

## Usage

Requirements: Go 1.26 (the toolchain is fetched automatically).

```bash
go build -o bin/promql-compat ./cmd/promql-compat

# Download a GreptimeDB release, start a private standalone instance on ports 4600-4603 and run
# all suites.
./bin/promql-compat --greptime-version v1.2.1

# Same with a local binary, or against a running instance.
./bin/promql-compat --greptime-bin /path/to/greptime --label my-branch
./bin/promql-compat --greptime-url http://127.0.0.1:4000

# VictoriaMetrics single-node, downloaded or local, on port 4600. The corpus suite is not
# supported (see Limitations).
./bin/promql-compat --vm-version v1.153.0 --suites compliance,fuzz
./bin/promql-compat --vm-bin /path/to/victoria-metrics-prod --suites compliance,fuzz

# Harness self-check: the reference against itself. Exits with status 1 if an in-scope case fails.
./bin/promql-compat --control
```

Output goes to `out/<timestamp>/` (`--out` to override): `REPORT.md` and `results.json`.

`--greptime-version` and `--vm-version` download the release tarball for the current OS and
architecture from GitHub, check it against the published sha256 and cache the binary under the
user cache directory (`promql-compat/<product>-<tag>-<os>-<arch>/`). Exactly one target flag is
required.

VictoriaMetrics is started with `-retentionPeriod=100y` (the demo dataset is dated 2026-02-28,
outside the default 1 month retention) and `-search.disableCache`. After loading a dataset the
harness calls `/internal/force_flush`; written samples are not searchable before that. The
compliance and fuzz suites share one load of the demo dataset.

| Flag | Default | Meaning |
|---|---|---|
| `--suites` | `corpus,compliance,fuzz` | Suites to run |
| `--files` | | Corpus: only files whose name contains this substring |
| `--fuzz-count`, `--fuzz-seed` | `2000`, `1` | Fuzz size and seed; the same seed and harness revision yield the same query sequence |
| `--shrink` | `true` | Fuzz: shrink failing queries (slow; does not affect rates) |
| `--parallel` | `8` | Concurrent cases (corpus: concurrent files) |
| `--port` | `4600` | First port of a started target: GreptimeDB uses 4 consecutive ports, VictoriaMetrics 1; the run aborts if any is busy |
| `--keep-data` | `false` | Keep the created databases and the data directory of a started target |

Other modes:

```bash
# Cases that flipped between two runs. Exits with status 1 if an in-scope case regressed.
./bin/promql-compat -compare out/a/results.json -against out/b/results.json

# Regenerate REPORT.md from results.json.
./bin/promql-compat --rerender out/a/results.json

# Print both results of queries on the demo dataset (time is an offset from the dataset start,
# or from..to/step), or on the load blocks of a promqltest file (times count from 0).
./bin/promql-compat --greptime-url URL --probe 'rate(demo_cpu_usage_seconds_total[1m])' --probe-time 52m30s
./bin/promql-compat --greptime-url URL --probe-data repro.test --probe 'max(m)' --probe-time 0s
```

## Report

`REPORT.md` gives, per suite, the rate under several criteria:

- **Strict**: every query of the case matches.
- **Primary query**: only the query as written (for the corpus, without the `@` variants and the
  range-mode self-check).
- **Ignore negative timestamps**: checks failing only because the evaluation time is before 1970
  count as passed.
- **Values only**: checks whose result differs only in `__name__` count as passed.

Passed cases are split by evidence: *data* (at least one query returned a non-empty result on
both sides), *empty only* and *errors only*. The rate counts all three as passed.

The coverage table lists, per function, aggregation and binary operator, the in-scope cases using
it as passed/total and the passed cases with data. Every non-experimental function is listed, so
functions without cases show up.

It also lists outcomes by status, failure causes, the most frequent failure signatures, results
per corpus file or query template, and every in-scope failure with its first failing query.

## Reproducing a run

`results.json` records the command line, the harness VCS revision (`+dirty` with uncommitted
changes; `unknown` in binaries built without VCS information), the Prometheus module version, the
suites, the corpus file filter, and the fuzz count and seed. The report header shows them.
`-compare` prints the fields that differ between the two runs, and per suite the cases present in
only one run or in scope in only one; those cases are not compared.

## Limitations

- VictoriaMetrics rejects samples before 1970-01-02, and the corpus loads its data from the epoch,
  so the `corpus` suite and `--probe-data` are not supported for it. Its HTTP API has no dataset
  isolation; each run starts a private instance.
- Native histogram results are decoded from the API JSON form, which drops the schema; these cases
  are excluded.
- The reference runs with Prometheus server defaults for `compliance` and `fuzz`: 5m lookback,
  1m default subquery step, no experimental functions.
- GreptimeDB reads `db` only from the URL query string, so the client always sends it there.

## License

Apache-2.0. `internal/corpus/promqltest.go` and `internal/corpus/testdata` come from
prometheus/prometheus; `internal/compliance/promql-test-queries.yml` comes from
prometheus/compliance.
