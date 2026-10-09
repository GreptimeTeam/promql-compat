// Package result holds the per-case outcomes shared by all suites.
package result

import "strings"

// Status of one check or case. Order matters: a case takes the worst status of
// its checks, and statuses later in the list are worse.
type Status string

const (
	Pass Status = "pass"
	// PassUnsorted: values match, but a matrix was not sorted by labels.
	PassUnsorted Status = "pass_unsorted"
	// PassErrorMessage: both sides failed, with different messages.
	PassErrorMessage Status = "pass_error_message"
	// FailValue: both sides returned a result, and the results differ.
	FailValue Status = "fail_value"
	// FailNoError: the reference failed, the target returned a result.
	FailNoError Status = "fail_no_error"
	// FailError: the reference returned a result, the target failed.
	FailError Status = "fail_error"
	// FailIngest: the target rejected the input data.
	FailIngest Status = "fail_ingest"
	// FailTransport: no valid API response (crash, timeout, bad JSON).
	FailTransport Status = "fail_transport"
)

var rank = map[Status]int{
	Pass: 0, PassUnsorted: 1, PassErrorMessage: 2, FailValue: 3,
	FailNoError: 4, FailError: 5, FailIngest: 6, FailTransport: 7,
}

// Worse returns the worse of a and b.
func Worse(a, b Status) Status {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Passed reports whether s counts as compatible. Unsorted matrices and
// differing error messages are reported but not counted as incompatible:
// clients do not depend on either.
func (s Status) Passed() bool { return rank[s] <= rank[PassErrorMessage] }

// Check is one query sent to the target.
type Check struct {
	Kind   string `json:"kind"` // instant, instant_at, range_mode, range
	Query  string `json:"query"`
	Time   string `json:"time"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Annotation holds a warning/info mismatch; it does not affect Status.
	Annotation string `json:"annotation,omitempty"`
	// Cause is a coarse root-cause class for failed checks, see Classify.
	Cause string `json:"cause,omitempty"`
	// Nondeterministic: two reference instances disagreed, so the reference
	// answer is not unique (topk ties, float summation order).
	Nondeterministic bool `json:"nondeterministic,omitempty"`
	// Evidence is what a passed check agreed on, see Evidence*.
	Evidence string `json:"evidence,omitempty"`
}

// Two empty results or two errors agree without exercising the evaluation,
// so passes are split by what was compared.
const (
	EvidenceData  = "data"
	EvidenceEmpty = "empty"
	EvidenceError = "error"
)

// EvidenceOf is the evidence of a passed check that compared a result.
func EvidenceOf(empty bool) string {
	if empty {
		return EvidenceEmpty
	}
	return EvidenceData
}

// Root-cause classes assigned mechanically; everything else is "other".
const (
	CauseMetricName  = "metric_name_label"  // result differs only in __name__
	CauseNegativeTS  = "negative_timestamp" // query time before 1970 rejected
	CauseServerCrash = "server_crash"       // connection dropped (panic)
	CauseOther       = "other"
)

// Classify assigns a cause from the check's own detail. Suites that can
// re-compare with __name__ removed set CauseMetricName before calling it.
func (c *Check) Classify() {
	if c.Status.Passed() || c.Cause != "" {
		return
	}
	switch {
	case strings.Contains(c.Detail, "value is negative"):
		c.Cause = CauseNegativeTS
	case c.Status == FailTransport && (strings.HasSuffix(c.Detail, ": EOF") || strings.Contains(c.Detail, "connection reset")):
		c.Cause = CauseServerCrash
	default:
		c.Cause = CauseOther
	}
}

// Case is one test case, the unit of the compatibility rate.
type Case struct {
	Suite  string   `json:"suite"`
	ID     string   `json:"id"`
	Group  string   `json:"group"` // e.g. corpus file or query template
	Expr   string   `json:"expr"`
	Tags   []string `json:"tags,omitempty"`
	Status Status   `json:"status"`
	Checks []Check  `json:"checks"`
	// Repro is a self-contained reproduction (data + query), if available.
	Repro string `json:"repro,omitempty"`
	// Minimized is the smallest query found that fails the same way.
	Minimized string `json:"minimized,omitempty"`
}

// Finish sets the case status from its checks.
func (c *Case) Finish() {
	c.Status = Pass
	nondet := false
	for i := range c.Checks {
		c.Checks[i].Classify()
		c.Status = Worse(c.Status, c.Checks[i].Status)
		nondet = nondet || c.Checks[i].Nondeterministic
	}
	if nondet && !c.HasTag(TagNondeterministic) {
		c.Tags = append(c.Tags, TagNondeterministic)
	}
}

func (c *Case) HasTag(t string) bool {
	for _, x := range c.Tags {
		if x == t {
			return true
		}
	}
	return false
}

// Evidence is the strongest evidence among the checks: data if any check
// compared data, else empty if any compared an empty result, else error.
func (c *Case) Evidence() string {
	empty, errs := false, false
	for _, ch := range c.Checks {
		switch ch.Evidence {
		case EvidenceData:
			return EvidenceData
		case EvidenceEmpty:
			empty = true
		case EvidenceError:
			errs = true
		}
	}
	switch {
	case empty:
		return EvidenceEmpty
	case errs:
		return EvidenceError
	}
	return ""
}

// Causes returns the distinct causes of the failed checks.
func (c *Case) Causes() []string {
	var out []string
	seen := map[string]bool{}
	for _, ch := range c.Checks {
		if !ch.Status.Passed() && !seen[ch.Cause] {
			seen[ch.Cause] = true
			out = append(out, ch.Cause)
		}
	}
	return out
}

// Tags excluded from the main rate: features GreptimeDB does not implement
// by design, or that Prometheus itself gates behind feature flags.
const (
	TagNativeHistogram  = "native_histogram"
	TagStartTimestamp   = "start_timestamp"
	TagExperimental     = "experimental"
	TagDelayedName      = "delayed_name_removal"
	TagNondeterministic = "nondeterministic"
)

// Excluded reports whether the case is outside the main compatibility rate.
func (c *Case) Excluded() bool {
	for _, t := range c.Tags {
		switch t {
		case TagNativeHistogram, TagStartTimestamp, TagExperimental, TagDelayedName, TagNondeterministic:
			return true
		}
	}
	return false
}
