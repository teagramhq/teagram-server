// Package perfreport judges the RPC performance harness's measurements.
//
// Deterministic counters block from the first run: statements per RPC, rows
// read, sequential scans, and an identical statement count at 1x and 100x.
// Timings (the absolute budget, 1x against 100x, and base against head) are
// reported on every run but block only once main's history has given the
// scenario a noise band.
package perfreport

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Version is the report format. A base report in another format is not
// compared.
const Version = 1

// Fixture names.
const (
	Small = "1x"
	Large = "100x"
)

// Kind selects a scenario's latency budget.
type Kind string

// Scenario kinds.
const (
	Read   Kind = "read"
	Write  Kind = "write"
	FanOut Kind = "fanout"
)

// budgetMs is the p95 ceiling on the 100x fixture.
var budgetMs = map[Kind]float64{Read: 20, Write: 50, FanOut: 2000}

// Thresholds. Scale invariance allows the 100x run 1.5 times the 1x one;
// against base a run may be 15% slower. The absolute slack keeps timer and
// scheduler noise on sub-millisecond RPCs from deciding either.
const (
	scaleRatio      = 1.5
	regressionRatio = 1.15
	scaleSlackMs    = 1.0
	baseMinMs       = 2.0
	noiseSigmas     = 3
	rowsSlack       = 50
	allocSlack      = 256 << 10
)

// Calibration is how many main runs a scenario needs before its timings block.
const Calibration = 20

// Report is one harness run over both fixtures.
type Report struct {
	Version int      `json:"version"`
	Results []Result `json:"results"`
}

// Result is one scenario on one fixture.
type Result struct {
	Name    string `json:"name"`
	Fixture string `json:"fixture"`
	Kind    Kind   `json:"kind"`
	// FullSet marks an RPC whose answer is the whole collection by protocol
	// (contacts.getContacts). Its latency and rows grow with the collection, so
	// only its query count is held to scale invariance.
	FullSet bool    `json:"full_set,omitempty"`
	Samples int     `json:"samples"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	// Queries is the median statement count per call, including the delivery
	// work the call triggered after its reply.
	Queries int `json:"queries"`
	// RowsRead sums, over every scan node of every statement, the rows the
	// scan produced plus the rows its filter discarded.
	RowsRead   int64 `json:"rows_read"`
	AllocBytes int64 `json:"alloc_bytes"`
	// SeqScans names each sequential scan over a table above the size limit.
	SeqScans []string `json:"seq_scans,omitempty"`
	// Unexplained lists statements Postgres could not EXPLAIN. Their scans
	// and rows are unverified, so each one fails the run.
	Unexplained []string `json:"unexplained,omitempty"`
	Note        string   `json:"note,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Failure is one failed check.
type Failure struct {
	Scenario string
	Check    string
	Reason   string
	// Issue is the waiver's tracking issue when the failure is known.
	Issue string
}

// Checks a waiver may name. Regression against base is not among them: a
// known violation may stay, it may not get worse.
const (
	CheckBudget  = "budget"
	CheckQueries = "queries"
	CheckLatency = "latency"
	CheckRows    = "rows"
	CheckAlloc   = "alloc"
	CheckSeqScan = "seqscan"
	CheckError   = "error"
	// CheckUnexplained fails a statement whose plan could not be read.
	CheckUnexplained = "unexplained"
	checkMissing     = "missing"
	checkBase        = "regression"
	checkSlower      = "slower"
)

// timing reports whether a check is decided by a clock rather than a counter.
func timing(check string) bool {
	switch check {
	case CheckBudget, CheckLatency, CheckAlloc, checkSlower:
		return true
	}
	return false
}

// Waiver accepts one check failing on one scenario until Issue fixes it.
type Waiver struct {
	Scenario string `json:"scenario"`
	Check    string `json:"check"`
	Issue    string `json:"issue"`
	// Scan names the one sequential scan a seqscan waiver accepts, as
	// "table#fingerprint" from the report. Any other scan still fails.
	Scan string `json:"scan,omitempty"`
}

// ParseWaivers reads the known-violations list.
func ParseWaivers(raw []byte) ([]Waiver, error) {
	var ws []Waiver
	if err := json.Unmarshal(raw, &ws); err != nil {
		return nil, err
	}
	for _, w := range ws {
		switch w.Check {
		case CheckBudget, CheckQueries, CheckLatency, CheckRows, CheckAlloc, CheckSeqScan, CheckError, CheckUnexplained:
		default:
			return nil, fmt.Errorf("waiver for %q: check %q cannot be waived", w.Scenario, w.Check)
		}
		if w.Scenario == "" || w.Issue == "" {
			return nil, fmt.Errorf("waiver %+v needs a scenario and a tracking issue", w)
		}
		if (w.Check == CheckSeqScan) != (w.Scan != "") {
			return nil, fmt.Errorf("waiver %+v: a seqscan waiver, and only one, names its scan", w)
		}
	}
	return ws, nil
}

// Row is one scenario aggregated over every round.
type Row struct {
	Name  string
	Kind  Kind
	Small *Result
	Large *Result
	Base  *Result
	// Band is the standard deviation of main's 100x p50 over Runs main runs.
	Band   float64
	Runs   int
	Failed bool
	// Reported is set when a timing check missed while uncalibrated.
	Reported bool
	// Known lists the issues of the waived failures on this row.
	Known []string
}

// Verdict is the outcome of Evaluate.
type Verdict struct {
	Rows []Row
	// Failures are deterministic checks that failed; they block.
	Failures []Failure
	// TimingFailures are timing checks that failed on a calibrated scenario;
	// they block once a rerun confirms them.
	TimingFailures []Failure
	// Reported are timing checks that failed before calibration.
	Reported []Failure
	// Waived are failures a waiver accepts; they do not fail the run.
	Waived []Failure
	// Stale are waivers whose check passed: the issue may be fixed.
	Stale      []Waiver
	Rounds     int
	BaseRounds int
}

// Input is everything one judgement reads.
type Input struct {
	// Head holds the head branch's rounds.
	Head []Report
	// Base holds the base branch's rounds, interleaved with head's in the same
	// job. It is empty when the base branch has no harness yet.
	Base []Report
	// History holds one report per earlier main run, for noise bands.
	History []Report
	Waivers []Waiver
}

// Evaluate aggregates each scenario's rounds by median and applies every
// check.
func Evaluate(in Input) Verdict {
	v := Verdict{Rounds: len(in.Head), BaseRounds: len(in.Base)}
	headAgg, order := aggregate(in.Head)
	baseAgg, _ := aggregate(in.Base)
	bands := noise(in.History)
	// checked holds the scenarios measured on both fixtures, the only ones whose
	// unused waivers are stale.
	checked := map[string]bool{}
	used := map[Waiver]bool{}
	for _, name := range order {
		row := Row{Name: name, Small: headAgg[key{name, Small}], Large: headAgg[key{name, Large}], Base: baseAgg[key{name, Large}]}
		row.Band, row.Runs = bands[name].sd, bands[name].n
		calibrated := row.Runs >= Calibration
		// record files a failed check. scan is the fingerprint of a sequential
		// scan, which only a waiver naming that scan covers.
		record := func(check, scan string, format string, args ...any) {
			f := Failure{Scenario: name, Check: check, Reason: fmt.Sprintf(format, args...)}
			for _, w := range in.Waivers {
				if w.Scenario == name && w.Check == check && w.Scan == scan {
					used[w] = true
					f.Issue = w.Issue
					if !slices.Contains(row.Known, w.Issue) {
						row.Known = append(row.Known, w.Issue)
					}
					v.Waived = append(v.Waived, f)
					return
				}
			}
			switch {
			case !timing(check):
				row.Failed = true
				v.Failures = append(v.Failures, f)
			case calibrated && check != CheckAlloc:
				row.Failed = true
				v.TimingFailures = append(v.TimingFailures, f)
			default:
				row.Reported = true
				v.Reported = append(v.Reported, f)
			}
		}
		fail := func(check, format string, args ...any) { record(check, "", format, args...) }
		for _, r := range []*Result{row.Small, row.Large} {
			if r == nil {
				continue
			}
			row.Kind = r.Kind
			if r.Error != "" {
				fail(CheckError, "%s run failed: %s", r.Fixture, r.Error)
			}
			for _, scan := range r.SeqScans {
				record(CheckSeqScan, scanKey(scan), "sequential scan on %s at %s", scan, r.Fixture)
			}
			for _, sql := range r.Unexplained {
				fail(CheckUnexplained, "could not EXPLAIN %s at %s", sql, r.Fixture)
			}
		}
		if row.Small == nil {
			fail(checkMissing, "no %s measurement", Small)
		}
		if row.Large == nil {
			fail(checkMissing, "no %s measurement", Large)
		}
		if row.Small == nil || row.Large == nil || row.Small.Error != "" || row.Large.Error != "" {
			v.Rows = append(v.Rows, row)
			continue
		}
		s, l := row.Small, row.Large
		if budget := budgetMs[l.Kind]; l.P95Ms > budget {
			fail(CheckBudget, "p95 %.1f ms at %s, budget %.0f ms", l.P95Ms, Large, budget)
		}
		if s.Queries != l.Queries {
			fail(CheckQueries, "queries %d at %s, %d at %s", s.Queries, Small, l.Queries, Large)
		}
		if !l.FullSet {
			if l.P50Ms > scaleRatio*s.P50Ms && l.P50Ms-s.P50Ms > scaleSlackMs {
				fail(CheckLatency, "p50 %.1f ms at %s is %.1fx the %s %.1f ms", l.P50Ms, Large, l.P50Ms/s.P50Ms, Small, s.P50Ms)
			}
			if float64(l.RowsRead) > scaleRatio*float64(s.RowsRead) && l.RowsRead-s.RowsRead > rowsSlack {
				fail(CheckRows, "rows read %d at %s is %.1fx the %s %d", l.RowsRead, Large, ratio(l.RowsRead, s.RowsRead), Small, s.RowsRead)
			}
			if float64(l.AllocBytes) > scaleRatio*float64(s.AllocBytes) && l.AllocBytes-s.AllocBytes > allocSlack {
				fail(CheckAlloc, "allocated %s at %s is %.1fx the %s %s", bytes(l.AllocBytes), Large, ratio(l.AllocBytes, s.AllocBytes), Small, bytes(s.AllocBytes))
			}
		}
		if b := row.Base; b != nil && b.Error == "" {
			allowed := max((regressionRatio-1)*b.P50Ms, noiseSigmas*row.Band)
			if l.P50Ms-b.P50Ms > allowed && l.P50Ms-b.P50Ms >= baseMinMs {
				fail(checkSlower, "p50 %.1f ms at %s is %.0f%% slower than main's %.1f ms", l.P50Ms, Large, 100*(l.P50Ms/b.P50Ms-1), b.P50Ms)
			}
			if float64(l.Queries) > regressionRatio*float64(b.Queries) {
				fail(checkBase, "queries %d at %s, main runs %d", l.Queries, Large, b.Queries)
			}
			if float64(l.RowsRead) > regressionRatio*float64(b.RowsRead) && l.RowsRead-b.RowsRead > rowsSlack {
				fail(checkBase, "rows read %d at %s, main reads %d", l.RowsRead, Large, b.RowsRead)
			}
		}
		v.Rows = append(v.Rows, row)
		checked[name] = true
	}
	for _, w := range in.Waivers {
		if checked[w.Scenario] && !used[w] {
			v.Stale = append(v.Stale, w)
		}
	}
	return v
}

type key struct{ name, fixture string }

// scanKey is a scan's "table#fingerprint", the part a waiver names; the table
// size after it moves with the data.
func scanKey(scan string) string {
	k, _, _ := strings.Cut(scan, " ")
	return k
}

// Aggregate folds rounds into one report of their medians, the form a main
// run is kept in for noise bands.
func Aggregate(reports []Report) Report {
	agg, order := aggregate(reports)
	out := Report{Version: Version}
	for _, name := range order {
		for _, fixture := range []string{Small, Large} {
			if r := agg[key{name, fixture}]; r != nil {
				out.Results = append(out.Results, *r)
			}
		}
	}
	return out
}

type band struct {
	sd float64
	n  int
}

// noise is each scenario's standard deviation of the 100x p50 across main
// runs.
func noise(history []Report) map[string]band {
	samples := map[string][]float64{}
	for _, rep := range history {
		if rep.Version != Version {
			continue
		}
		for _, r := range rep.Results {
			if r.Fixture == Large && r.Error == "" {
				samples[r.Name] = append(samples[r.Name], r.P50Ms)
			}
		}
	}
	out := make(map[string]band, len(samples))
	for name, xs := range samples {
		var mean float64
		for _, x := range xs {
			mean += x
		}
		mean /= float64(len(xs))
		var ss float64
		for _, x := range xs {
			ss += (x - mean) * (x - mean)
		}
		sd := 0.0
		if len(xs) > 1 {
			sd = math.Sqrt(ss / float64(len(xs)-1))
		}
		out[name] = band{sd: sd, n: len(xs)}
	}
	return out
}

func aggregate(reports []Report) (map[key]*Result, []string) {
	byKey := map[key][]Result{}
	var order []string
	for _, rep := range reports {
		if rep.Version != Version {
			continue
		}
		for _, r := range rep.Results {
			k := key{r.Name, r.Fixture}
			if !slices.Contains(order, r.Name) {
				order = append(order, r.Name)
			}
			byKey[k] = append(byKey[k], r)
		}
	}
	out := make(map[key]*Result, len(byKey))
	for k, rs := range byKey {
		agg := rs[0]
		agg.P50Ms = median(rs, func(r Result) float64 { return r.P50Ms })
		agg.P95Ms = median(rs, func(r Result) float64 { return r.P95Ms })
		agg.Queries = int(median(rs, func(r Result) float64 { return float64(r.Queries) }))
		agg.RowsRead = int64(median(rs, func(r Result) float64 { return float64(r.RowsRead) }))
		agg.AllocBytes = int64(median(rs, func(r Result) float64 { return float64(r.AllocBytes) }))
		agg.SeqScans, agg.Unexplained, agg.Error = nil, nil, ""
		for _, r := range rs {
			for _, scan := range r.SeqScans {
				if !slices.Contains(agg.SeqScans, scan) {
					agg.SeqScans = append(agg.SeqScans, scan)
				}
			}
			if agg.Error == "" {
				agg.Error = r.Error
			}
			for _, sql := range r.Unexplained {
				if !slices.Contains(agg.Unexplained, sql) {
					agg.Unexplained = append(agg.Unexplained, sql)
				}
			}
		}
		out[k] = &agg
	}
	return out, order
}

// median takes the lower middle value, so an even count never reports a
// value no round measured.
func median(rs []Result, f func(Result) float64) float64 {
	vals := make([]float64, len(rs))
	for i, r := range rs {
		vals[i] = f(r)
	}
	slices.Sort(vals)
	return vals[(len(vals)-1)/2]
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return float64(a)
	}
	return float64(a) / float64(b)
}

func bytes(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
}

// Markdown renders the verdict as the CI summary table.
func Markdown(v Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## RPC performance\n\n")
	base := "no base measurement (base branch has no harness, or its fixture could not be prepared)"
	if v.BaseRounds > 0 {
		base = fmt.Sprintf("%d interleaved base rounds", v.BaseRounds)
	}
	fmt.Fprintf(&b, "Medians of %d rounds; %s. Budgets are p95 on the 100x fixture. "+
		"Query counts, rows read and sequential scans block; timings block once %d main runs have set the scenario's noise band.\n\n",
		v.Rounds, base, Calibration)
	b.WriteString("| Scenario | Kind | p50 1x (ms) | p50 100x (ms) | p95 100x (ms) | Budget (ms) | Noise band | Queries 1x / 100x | Rows read 1x / 100x | Alloc 100x | p50 vs main | Status |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, row := range v.Rows {
		status := "pass"
		if row.Reported {
			status = "timing reported"
		}
		if len(row.Known) > 0 {
			status = "known: " + strings.Join(row.Known, ", ")
		}
		if row.Failed {
			status = "**FAIL**"
			if len(row.Known) > 0 {
				status += " (known: " + strings.Join(row.Known, ", ") + ")"
			}
		}
		if row.Small == nil || row.Large == nil {
			fmt.Fprintf(&b, "| %s | %s | | | | | | | | | | %s |\n", row.Name, row.Kind, status)
			continue
		}
		s, l := row.Small, row.Large
		vsMain := "n/a"
		if row.Base != nil && row.Base.P50Ms > 0 {
			vsMain = fmt.Sprintf("%+.0f%%", 100*(l.P50Ms/row.Base.P50Ms-1))
		}
		name := row.Name
		if l.FullSet {
			name += " (full set)"
		}
		bandCell := fmt.Sprintf("calibrating %d/%d", row.Runs, Calibration)
		if row.Runs >= Calibration {
			bandCell = fmt.Sprintf("±%.1f ms (%d runs)", row.Band, row.Runs)
		}
		fmt.Fprintf(&b, "| %s | %s | %.1f | %.1f | %.1f | %.0f | %s | %d / %d | %d / %d | %s | %s | %s |\n",
			name, l.Kind, s.P50Ms, l.P50Ms, l.P95Ms, budgetMs[l.Kind], bandCell, s.Queries, l.Queries,
			s.RowsRead, l.RowsRead, bytes(l.AllocBytes), vsMain, status)
	}
	b.WriteString("\n")
	if len(v.Failures) == 0 && len(v.TimingFailures) == 0 {
		b.WriteString("All blocking checks passed.\n")
	}
	list := func(title string, fs []Failure) {
		if len(fs) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d):\n\n", title, len(fs))
		for _, f := range fs {
			fmt.Fprintf(&b, "- `%s`: %s\n", f.Scenario, f.Reason)
		}
	}
	list("Failed checks", v.Failures)
	list("Failed timing checks", v.TimingFailures)
	list("Timing findings, reported until calibrated", v.Reported)
	if len(v.Waived) > 0 {
		fmt.Fprintf(&b, "\n%d known violations, each tracked by its issue:\n\n", len(v.Waived))
		for _, f := range v.Waived {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", f.Scenario, f.Issue, f.Reason)
		}
	}
	var notes []string
	for _, w := range v.Stale {
		notes = append(notes, fmt.Sprintf("- `%s` %s (%s) no longer fails; remove its entry from known_violations.json", w.Scenario, w.Check, w.Issue))
	}
	for _, row := range v.Rows {
		if row.Large != nil && row.Large.Note != "" {
			notes = append(notes, fmt.Sprintf("- `%s`: %s", row.Name, row.Large.Note))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\nNotes:\n\n" + strings.Join(notes, "\n") + "\n")
	}
	return b.String()
}
