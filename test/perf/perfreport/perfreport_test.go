package perfreport_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/test/perf/perfreport"
)

func result(p50, p95 float64, queries int, rows int64) perfreport.Result {
	return perfreport.Result{P50Ms: p50, P95Ms: p95, Queries: queries, RowsRead: rows, AllocBytes: 1 << 20}
}

func report(results ...perfreport.Result) perfreport.Report {
	return perfreport.Report{Version: perfreport.Version, FixtureVersion: "fixture-test-v2", Results: results}
}

func withFixtureVersion(r perfreport.Report, version string) perfreport.Report {
	r.FixtureVersion = version
	return r
}

func pair(name string, kind perfreport.Kind, small, large perfreport.Result) []perfreport.Result {
	small.Name, large.Name = name, name
	small.Fixture, large.Fixture = perfreport.Small, perfreport.Large
	small.Kind, large.Kind = kind, kind
	return []perfreport.Result{small, large}
}

func rounds(rs ...perfreport.Result) []perfreport.Report { return []perfreport.Report{report(rs...)} }

// calibrated is main's history for one scenario: n runs whose 100x p50
// alternates around p50 by jitter.
func calibrated(name string, n int, p50, jitter float64) []perfreport.Report {
	reps := make([]perfreport.Report, 0, n)
	for i := range n {
		x := p50 + jitter
		if i%2 == 1 {
			x = p50 - jitter
		}
		reps = append(reps, report(perfreport.Result{Name: name, Fixture: perfreport.Large, P50Ms: x}))
	}
	return reps
}

func reasons(fs []perfreport.Failure) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Scenario + ": " + f.Reason + "\n")
	}
	return b.String()
}

func TestEvaluatePassesInvariantScenarioWithinBudget(t *testing.T) {
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(pair("messages.getDialogs", perfreport.Read,
		result(2, 3, 5, 120), result(2.5, 4, 5, 130))...)})
	if len(v.Failures)+len(v.TimingFailures)+len(v.Reported) != 0 {
		t.Fatalf("failures = %s%s%s", reasons(v.Failures), reasons(v.TimingFailures), reasons(v.Reported))
	}
}

func TestEvaluateBlocksQueryCountThatGrowsWithData(t *testing.T) {
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(pair("messages.getDialogs", perfreport.Read,
		result(2, 3, 5, 120), result(2, 3, 104, 120))...)})
	if got := reasons(v.Failures); !strings.Contains(got, "messages.getDialogs: queries 5 at 1x, 104 at 100x") {
		t.Fatalf("failures = %q, want the query-count growth", got)
	}
}

func TestEvaluateBlocksRowsThatFollowAccountSize(t *testing.T) {
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(pair("contacts.search", perfreport.Read,
		result(2, 3, 4, 100), result(2, 3, 4, 10_000))...)})
	if got := reasons(v.Failures); !strings.Contains(got, "contacts.search: rows read 10000 at 100x is 100.0x the 1x 100") {
		t.Fatalf("failures = %q", got)
	}
}

func TestEvaluateBlocksSequentialScan(t *testing.T) {
	rs := pair("messages.search", perfreport.Read, result(1, 2, 3, 10), result(1, 2, 3, 10))
	rs[1].SeqScans = []string{"messages#1a2b3c4d (1000000 rows): SELECT id FROM messages"}
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(rs...)})
	if got := reasons(v.Failures); !strings.Contains(got, "messages.search: sequential scan on messages#1a2b3c4d (1000000 rows): SELECT id FROM messages at 100x") {
		t.Fatalf("failures = %q, want the sequential scan", got)
	}
}

func TestEvaluateBlocksCounterRegressionAgainstBase(t *testing.T) {
	base := rounds(pair("messages.getHistory", perfreport.Read, result(4, 5, 5, 100), result(4, 5, 5, 100))...)
	head := rounds(pair("messages.getHistory", perfreport.Read, result(4, 5, 6, 100), result(4, 5, 6, 400))...)
	got := reasons(perfreport.Evaluate(perfreport.Input{Head: head, Base: base}).Failures)
	for _, want := range []string{
		"messages.getHistory: queries 6 at 100x, main runs 5",
		"messages.getHistory: rows read 400 at 100x, main reads 100",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failures = %q, want %q", got, want)
		}
	}
}

func TestEvaluateReportsBaseRelativeTimingUntilCalibrated(t *testing.T) {
	base := rounds(pair("read", perfreport.Read,
		result(2, 3, 5, 100), result(10, 21, 5, 100))...)
	head := rounds(pair("read", perfreport.Read,
		result(2, 3, 5, 100), result(12.5, 25, 5, 100))...)
	v := perfreport.Evaluate(perfreport.Input{
		Head: head, Base: base, History: calibrated("read", perfreport.Calibration-1, 10, 0.1),
	})
	if len(v.Failures)+len(v.TimingFailures) != 0 {
		t.Fatalf("uncalibrated timing blocked: %s%s", reasons(v.Failures), reasons(v.TimingFailures))
	}
	if got := reasons(v.Reported); !strings.Contains(got, "read: p50 12.5 ms at 100x is 25% slower than main's 10.0 ms") {
		t.Fatalf("reported = %q, want the uncalibrated base-relative regression", got)
	}
	if md := perfreport.Markdown(v); !strings.Contains(md, "calibrating 19/20") || !strings.Contains(md, "timing reported") {
		t.Errorf("markdown = %s", md)
	}
}

func TestEvaluateWaivesScaleLimitsButChecksBaseRelativeTiming(t *testing.T) {
	base := rounds(pair("read", perfreport.Read,
		result(2, 3, 5, 100), result(10, 40, 5, 100))...)
	head := rounds(pair("read", perfreport.Read,
		result(2, 3, 5, 100), result(10.1, 44, 5, 100))...)
	waivers := []perfreport.Waiver{{Scenario: "read", Check: perfreport.CheckTiming, Issue: "MAIN-2811"}}
	v := perfreport.Evaluate(perfreport.Input{
		Head: head, Base: base, History: calibrated("read", perfreport.Calibration, 10, 0.5), Waivers: waivers,
	})
	if got := reasons(v.TimingFailures); got != "" {
		t.Fatalf("base-relative timing failed despite a 1%% p50 change: %s", got)
	}
	if got := reasons(v.Reported); got != "" {
		t.Fatalf("calibrated base-relative timing was reported as uncalibrated: %s", got)
	}
	if len(v.Waived) != 2 {
		t.Fatalf("waived = %+v, want the p95 budget and scale-ratio checks", v.Waived)
	}
}

func TestEvaluateComparesMainTimingAgainstCalibratedHistory(t *testing.T) {
	head := rounds(pair("read", perfreport.Read,
		result(13, 15, 5, 100), result(13.5, 16, 5, 100))...)
	v := perfreport.Evaluate(perfreport.Input{
		Head: head, History: calibrated("read", perfreport.Calibration, 10, 0.5),
	})
	if got := reasons(v.TimingFailures); !strings.Contains(got, "read: p50 13.5 ms at 100x is 42% slower than main history's 9.5 ms") {
		t.Fatalf("timing failures = %q, want main-history regression", got)
	}
}

func TestEvaluateIgnoresHistoryWithoutMatchingFixtureVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{
		{name: "missing provenance"},
		{name: "different fixture", version: "fixture-old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := rounds(pair("read", perfreport.Read,
				result(13, 15, 5, 100), result(13.5, 16, 5, 100))...)
			head[0] = withFixtureVersion(head[0], "fixture-current")
			history := calibrated("read", perfreport.Calibration, 9.5, 0.5)
			for i := range history {
				history[i] = withFixtureVersion(history[i], tc.version)
			}

			v := perfreport.Evaluate(perfreport.Input{Head: head, History: history})
			if len(v.TimingFailures) != 0 {
				t.Fatalf("old fixture history caused timing failures: %s", reasons(v.TimingFailures))
			}
			if got := v.Rows[0].Runs; got != 0 {
				t.Fatalf("calibrated history runs = %d, want no matching fixture history", got)
			}
		})
	}
}

func TestAggregatePreservesFixtureVersion(t *testing.T) {
	r := rounds(pair("read", perfreport.Read,
		result(2, 3, 5, 100), result(2, 3, 5, 100))...)[0]
	r = withFixtureVersion(r, "fixture-current")
	raw, err := json.Marshal(perfreport.Aggregate([]perfreport.Report{r}))
	if err != nil {
		t.Fatal(err)
	}
	var aggregated struct {
		FixtureVersion string `json:"fixture_version"`
	}
	if err := json.Unmarshal(raw, &aggregated); err != nil {
		t.Fatal(err)
	}
	if aggregated.FixtureVersion != "fixture-current" {
		t.Fatalf("fixture version = %q, want fixture-current", aggregated.FixtureVersion)
	}
}

func TestEvaluateWaivesKnownTimingChecksOnlyForNamedScenario(t *testing.T) {
	known := "messages.search"
	waivers := []perfreport.Waiver{{Scenario: known, Check: perfreport.CheckTiming, Issue: "MAIN-2578"}}
	base := rounds(pair(known, perfreport.Read, result(2, 3, 3, 100), result(2, 3, 3, 100))...)
	head := rounds(pair(known, perfreport.Read, result(2, 3, 3, 100), result(6, 25, 3, 100))...)
	v := perfreport.Evaluate(perfreport.Input{
		Head: head, Base: base, History: calibrated(known, perfreport.Calibration, 2, 0.1), Waivers: waivers,
	})
	if len(v.Waived) != 2 {
		t.Fatalf("waived = %+v, want the budget and scale-ratio checks", v.Waived)
	}
	waivedChecks := map[string]bool{}
	for _, f := range v.Waived {
		waivedChecks[f.Check] = true
		if f.Issue != "MAIN-2578" {
			t.Errorf("waiver issue = %q, want MAIN-2578", f.Issue)
		}
	}
	if !waivedChecks[perfreport.CheckBudget] || !waivedChecks[perfreport.CheckLatency] {
		t.Errorf("waived checks = %v, want budget and scale-ratio", waivedChecks)
	}
	if len(v.Failures) != 0 {
		t.Errorf("deterministic failures = %s", reasons(v.Failures))
	}
	if got := reasons(v.TimingFailures); !strings.Contains(got, "slower than main") || strings.Contains(got, "budget") || strings.Contains(got, "the 1x") {
		t.Errorf("timing failures = %q, want only the unwaived base regression", got)
	}

	counterRegression := pair(known, perfreport.Read, result(2, 3, 3, 100), result(6, 25, 4, 100))
	v = perfreport.Evaluate(perfreport.Input{Head: rounds(counterRegression...), Waivers: waivers})
	if got := reasons(v.Failures); !strings.Contains(got, "queries 3 at 1x, 4 at 100x") {
		t.Errorf("deterministic failures = %q, want query-count growth to block", got)
	}

	untracked := "upload.getFile"
	untrackedHead := rounds(pair(untracked, perfreport.Read,
		result(2, 3, 3, 100), result(6, 25, 3, 100))...)
	v = perfreport.Evaluate(perfreport.Input{
		Head: untrackedHead, History: calibrated(untracked, perfreport.Calibration, 2, 0.1), Waivers: waivers,
	})
	got := reasons(v.TimingFailures)
	if !strings.Contains(got, "budget") || !strings.Contains(got, "the 1x") || !strings.Contains(got, "slower than main history") || len(v.TimingFailures) != 3 {
		t.Fatalf("untracked timing failures = %q, want budget, scale-ratio and baseline checks to block", got)
	}
}

func TestEvaluateMarksPassingTimingWaiverStale(t *testing.T) {
	name := "updates.getDifference 30 days offline"
	waiver := perfreport.Waiver{Scenario: name, Check: perfreport.CheckTiming, Issue: "MAIN-2580"}
	head := rounds(pair(name, perfreport.Read,
		result(2, 3, 5, 100), result(2.5, 4, 5, 100))...)
	v := perfreport.Evaluate(perfreport.Input{Head: head, Waivers: []perfreport.Waiver{waiver}})
	if len(v.Failures)+len(v.TimingFailures)+len(v.Reported) != 0 {
		t.Fatalf("passing timing waiver produced failures: %s%s%s", reasons(v.Failures), reasons(v.TimingFailures), reasons(v.Reported))
	}
	if len(v.Stale) != 1 || v.Stale[0] != waiver {
		t.Fatalf("stale waivers = %+v, want %+v", v.Stale, waiver)
	}
	md := perfreport.Markdown(v)
	if !strings.Contains(md, "`updates.getDifference 30 days offline` timing (MAIN-2580) no longer fails; remove its entry from known_violations.json") {
		t.Fatalf("markdown = %s", md)
	}
}

func TestEvaluateSlowerThanBaseNeedsNoiseBandAndTwoMilliseconds(t *testing.T) {
	base := rounds(pair("h", perfreport.Read, result(10, 12, 5, 100), result(10, 12, 5, 100))...)
	history := calibrated("h", perfreport.Calibration, 10, 1)
	for _, tc := range []struct {
		p50  float64
		fail bool
	}{
		{11.9, false}, // 19% slower but under 2 ms
		{13.0, false}, // 30% slower, inside 3 sigma (3.08 ms)
		{13.5, true},
	} {
		head := rounds(pair("h", perfreport.Read, result(tc.p50, 12, 5, 100), result(tc.p50, 12, 5, 100))...)
		v := perfreport.Evaluate(perfreport.Input{Head: head, Base: base, History: history})
		if failed := strings.Contains(reasons(v.TimingFailures), "slower than main"); failed != tc.fail {
			t.Errorf("p50 %.1f against 10: failed = %v, want %v (%s)", tc.p50, failed, tc.fail, reasons(v.TimingFailures))
		}
	}
}

func TestEvaluateExemptsFullSetScenarioFromSizeInvarianceOnly(t *testing.T) {
	rs := pair("contacts.getContacts", perfreport.Read, result(1, 2, 3, 100), result(10, 15, 3, 10_000))
	rs[0].FullSet, rs[1].FullSet = true, true
	if v := perfreport.Evaluate(perfreport.Input{Head: rounds(rs...)}); len(v.Failures)+len(v.Reported) != 0 {
		t.Fatalf("failures = %s%s", reasons(v.Failures), reasons(v.Reported))
	}
	rs[1].Queries = 4
	if v := perfreport.Evaluate(perfreport.Input{Head: rounds(rs...)}); len(v.Failures) != 1 {
		t.Fatalf("failures = %s, want the query-count change", reasons(v.Failures))
	}
}

func TestEvaluateTakesMedianAcrossRounds(t *testing.T) {
	head := make([]perfreport.Report, 0, 3)
	for _, q := range []int{5, 40, 5} {
		head = append(head, report(pair("messages.getHistory", perfreport.Read, result(2, 3, 5, 100), result(2, 3, q, 100))...))
	}
	if v := perfreport.Evaluate(perfreport.Input{Head: head}); len(v.Failures) != 0 {
		t.Fatalf("one odd round failed the gate: %s", reasons(v.Failures))
	}
}

func TestEvaluateBlocksMissingFixtureAndHarnessError(t *testing.T) {
	missing := perfreport.Result{Name: "messages.getHistory", Fixture: perfreport.Large, Kind: perfreport.Read}
	rs := pair("upload.getFile", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))
	rs[1].Error = "FILE_REFERENCE_EXPIRED"
	got := reasons(perfreport.Evaluate(perfreport.Input{Head: rounds(append(rs, missing)...)}).Failures)
	for _, want := range []string{"messages.getHistory: no 1x measurement", "upload.getFile: 100x run failed: FILE_REFERENCE_EXPIRED"} {
		if !strings.Contains(got, want) {
			t.Errorf("failures = %q, want %q", got, want)
		}
	}
}

func TestEvaluateWaivesKnownViolationButNotRegression(t *testing.T) {
	base := rounds(pair("messages.getHistory", perfreport.Read, result(20, 25, 5, 100), result(20, 25, 105, 100))...)
	head := rounds(pair("messages.getHistory", perfreport.Read, result(20, 25, 5, 100), result(20, 25, 125, 100))...)
	waivers := []perfreport.Waiver{
		{Scenario: "messages.getHistory", Check: perfreport.CheckQueries, Issue: "MAIN-2"},
		{Scenario: "messages.getHistory", Check: perfreport.CheckSeqScan, Issue: "MAIN-3", Scan: "messages#00000000"},
	}
	v := perfreport.Evaluate(perfreport.Input{Head: head, Base: base, Waivers: waivers})
	got := reasons(v.Failures)
	if !strings.Contains(got, "queries 125 at 100x, main runs 105") {
		t.Fatalf("failures = %q, want the regression to stay a failure", got)
	}
	if strings.Contains(got, "queries 5 at 1x") || len(v.Waived) != 1 {
		t.Fatalf("failures = %q, waived = %+v, want the query-count difference waived", got, v.Waived)
	}
	md := perfreport.Markdown(v)
	if !strings.Contains(md, "known: MAIN-2") || !strings.Contains(md, "`messages.getHistory` seqscan (MAIN-3) no longer fails") {
		t.Fatalf("markdown = %s", md)
	}
}

func TestParseWaiversRejectsUnwaivableChecks(t *testing.T) {
	for _, raw := range []string{
		`[{"scenario":"x","check":"regression","issue":"MAIN-1"}]`,
		`[{"scenario":"x","check":"slower","issue":"MAIN-1"}]`,
		`[{"scenario":"x","check":"timing"}]`,
	} {
		if _, err := perfreport.ParseWaivers([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if w, err := perfreport.ParseWaivers([]byte(`[{"scenario":"x","check":"seqscan","issue":"MAIN-1","scan":"messages#1a2b3c4d"},{"scenario":"y","check":"timing","issue":"MAIN-2"}]`)); err != nil || len(w) != 2 {
		t.Fatalf("waivers = %v, %v", w, err)
	}
}

func TestKnownViolationWaiversMatchTrackedIssues(t *testing.T) {
	raw, err := os.ReadFile("../known_violations.json")
	if err != nil {
		t.Fatal(err)
	}
	waivers, err := perfreport.ParseWaivers(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"messages.search":      "MAIN-2578",
		"contacts.getContacts": "MAIN-3015",
	}
	for scenario, issue := range want {
		found := false
		for _, waiver := range waivers {
			if waiver.Scenario == scenario && waiver.Check == perfreport.CheckTiming && waiver.Issue == issue {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("known_violations.json has no timing waiver for %s (%s)", scenario, issue)
		}
	}
	for _, removed := range []struct {
		scenario string
		check    string
	}{
		{scenario: "messages.getHistory oldest page", check: perfreport.CheckRows},
		{scenario: "messages.getHistory oldest page", check: perfreport.CheckSeqScan},
		{scenario: "messages.getHistory group", check: perfreport.CheckSeqScan},
	} {
		for _, waiver := range waivers {
			if waiver.Scenario == removed.scenario && waiver.Check == removed.check {
				t.Errorf("known_violations.json still has resolved %s waiver for %s", removed.check, removed.scenario)
			}
		}
	}
	for _, scenario := range []string{"contacts.search", "messages.getDialogs"} {
		for _, waiver := range waivers {
			if waiver.Scenario == scenario && waiver.Check == perfreport.CheckTiming {
				t.Errorf("known_violations.json still has a timing waiver for %s (%s)", scenario, waiver.Issue)
			}
		}
	}
	for _, scenario := range []string{
		"updates.getDifference 1 day offline",
		"updates.getDifference 30 days offline",
	} {
		for _, waiver := range waivers {
			if waiver.Scenario == scenario {
				t.Errorf("known_violations.json still has a %s waiver for %s (%s)", waiver.Check, scenario, waiver.Issue)
			}
		}
	}
}

func TestMarkdownListsEveryScenarioWithBudgetAndBand(t *testing.T) {
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(pair("messages.getDialogs", perfreport.Read,
		result(2, 3, 5, 120), result(2.5, 4, 5, 130))...)})
	md := perfreport.Markdown(v)
	for _, want := range []string{
		"| messages.getDialogs | read | 2.0 | 2.5 | 4.0 | 20 | calibrating 0/20 | 5 / 5 | 120 / 130 | 1.0 MiB | n/a | pass |",
		"All blocking checks passed.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown = %s\nwant %q", md, want)
		}
	}
}

func TestMarkdownSeparatesPerformanceRegressionsFromRPCFailures(t *testing.T) {
	counters := pair("messages.getDialogs", perfreport.Read,
		result(2, 3, 5, 120), result(2, 3, 104, 120))
	rpcFailure := perfreport.Result{
		Name: "contacts.getContacts", Fixture: perfreport.Large,
		Kind: perfreport.Read, Error: "connection refused",
	}
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(append(counters, rpcFailure)...)})
	md := perfreport.Markdown(v)
	for _, want := range []string{
		"| contacts.getContacts | read | | | | | | | | | | **ERROR** |",
		"Performance regressions",
		"- `messages.getDialogs` (`queries`): queries 5 at 1x, 104 at 100x",
		"RPC or harness failures",
		"- `contacts.getContacts` (`error`): 100x run failed: connection refused",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown = %s\nwant %q", md, want)
		}
	}
}

func TestEvaluateWaivesOnlyTheNamedSequentialScan(t *testing.T) {
	known := "messages#1a2b3c4d (40200 rows): SELECT f.id FROM files f"
	added := "messages#99887766 (40200 rows): SELECT f.id FROM files f JOIN messages m"
	head := pair("upload.getFile", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))
	head[0].SeqScans = []string{"messages#1a2b3c4d (20100 rows): SELECT f.id FROM files f"}
	head[1].SeqScans = []string{known, added}
	waivers := []perfreport.Waiver{{Scenario: "upload.getFile", Check: perfreport.CheckSeqScan, Issue: "MAIN-2538", Scan: "messages#1a2b3c4d"}}
	// No base: the waiver alone must tell the known scan from the new one.
	v := perfreport.Evaluate(perfreport.Input{Head: rounds(head...), Waivers: waivers})
	got := reasons(v.Failures)
	if !strings.Contains(got, "sequential scan on "+added) {
		t.Fatalf("failures = %q, want the new scan to block despite the waiver", got)
	}
	if strings.Contains(got, "1a2b3c4d") || len(v.Waived) != 2 {
		t.Fatalf("failures = %q, waived = %+v, want the named scan waived at both fixtures", got, v.Waived)
	}
}

func TestParseWaiversRequiresScanForSeqScanOnly(t *testing.T) {
	for _, raw := range []string{
		`[{"scenario":"x","check":"seqscan","issue":"MAIN-1"}]`,
		`[{"scenario":"x","check":"rows","issue":"MAIN-1","scan":"messages#1a2b3c4d"}]`,
	} {
		if _, err := perfreport.ParseWaivers([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

func TestEvaluateBlocksUnexplainedStatements(t *testing.T) {
	rs := pair("messages.search", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))
	rs[1].Unexplained = []string{"SELECT id FROM messages WHERE id = $1"}
	got := reasons(perfreport.Evaluate(perfreport.Input{Head: rounds(rs...)}).Failures)
	if !strings.Contains(got, "messages.search: could not EXPLAIN SELECT id FROM messages WHERE id = $1 at 100x") {
		t.Fatalf("failures = %q", got)
	}
}

func TestEvaluateBlocksUnreadableOrEmptyHeadReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		head perfreport.Report
		want string
	}{
		{"empty", perfreport.Report{Version: perfreport.Version}, "head report 1 has no results"},
		{"old format", perfreport.Report{Version: perfreport.Version + 1, Results: pair("x", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))}, "head report 1 has format 3, want 2"},
	} {
		v := perfreport.Evaluate(perfreport.Input{Head: []perfreport.Report{tc.head}})
		if got := reasons(v.Failures); !strings.Contains(got, tc.want) {
			t.Errorf("%s: failures = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEvaluateBlocksScenarioTheHarnessListsButDidNotMeasure(t *testing.T) {
	head := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	head.Scenarios = []string{"help.getConfig", "messages.getDialogs"}
	got := reasons(perfreport.Evaluate(perfreport.Input{Head: []perfreport.Report{head}}).Failures)
	for _, want := range []string{"messages.getDialogs: no 1x measurement", "messages.getDialogs: no 100x measurement"} {
		if !strings.Contains(got, want) {
			t.Errorf("failures = %q, want %q", got, want)
		}
	}
}

func TestEvaluateBlocksScenarioDroppedFromBase(t *testing.T) {
	base := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	base.Scenarios = []string{"help.getConfig", "messages.getDialogs"}
	head := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	head.Scenarios = []string{"help.getConfig"}
	got := reasons(perfreport.Evaluate(perfreport.Input{Head: []perfreport.Report{head}, Base: []perfreport.Report{base}}).Failures)
	if !strings.Contains(got, "messages.getDialogs: measured on main, not on this branch") {
		t.Fatalf("failures = %q", got)
	}
}

func TestEvaluateBlocksRequiredScenarioMissingFromHarness(t *testing.T) {
	head := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	head.Scenarios = []string{"help.getConfig"}
	v := perfreport.Evaluate(perfreport.Input{Head: []perfreport.Report{head}, Required: []string{"help.getConfig", "messages.getDialogs"}})
	got := reasons(v.Failures)
	for _, want := range []string{
		"messages.getDialogs: required, but the harness does not run it",
		"messages.getDialogs: no 1x measurement",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failures = %q, want %q", got, want)
		}
	}
}

func TestEvaluateOmitsBaseScenarioRetiredFromRequiredManifest(t *testing.T) {
	const retired = "contacts.importContacts 5000"
	head := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	head.Scenarios = []string{"help.getConfig"}
	base := report(pair(retired, perfreport.Write, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	base.Scenarios = []string{retired}

	got := perfreport.Markdown(perfreport.Evaluate(perfreport.Input{
		Head: []perfreport.Report{head}, Base: []perfreport.Report{base}, Required: []string{"help.getConfig"},
	}))
	if strings.Contains(got, retired) {
		t.Fatalf("report contains retired scenario %q:\n%s", retired, got)
	}
}

func TestEvaluateBlocksFilteredRunWhenScenariosAreRequired(t *testing.T) {
	head := report(pair("help.getConfig", perfreport.Read, result(1, 1, 1, 1), result(1, 1, 1, 1))...)
	head.Scenarios, head.Filter = []string{"help.getConfig"}, "help"
	v := perfreport.Evaluate(perfreport.Input{Head: []perfreport.Report{head}, Required: []string{"help.getConfig"}})
	if got := reasons(v.Failures); !strings.Contains(got, `head report 1 ran only scenarios matching "help"`) {
		t.Fatalf("failures = %q", got)
	}
}
