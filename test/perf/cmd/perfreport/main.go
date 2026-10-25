// Command perfreport judges the RPC performance harness's JSON reports and
// prints the CI summary table. It exits 1 when a deterministic check fails and
// 3 when only calibrated timing checks fail, which the job reruns once.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/teagramhq/teagram-server/test/perf/perfreport"
)

func main() {
	head := flag.String("head", "", "glob of the head branch's reports")
	base := flag.String("base", "", "glob of the base branch's reports, interleaved with head's")
	history := flag.String("history", "", "glob of earlier main runs' aggregated reports")
	known := flag.String("waivers", "", "known-violations file")
	aggregate := flag.String("aggregate", "", "write head's rounds, folded into one report, to this file")
	flag.Parse()
	in := perfreport.Input{}
	var err error
	if in.Head, err = load(*head); err != nil || len(in.Head) == 0 {
		failf("no head reports at %q: %v", *head, err)
	}
	if in.Base, err = load(*base); err != nil {
		failf("base reports: %v", err)
	}
	if in.History, err = load(*history); err != nil {
		failf("history: %v", err)
	}
	if *known != "" {
		raw, err := os.ReadFile(*known)
		if err == nil {
			in.Waivers, err = perfreport.ParseWaivers(raw)
		}
		if err != nil {
			failf("waivers: %v", err)
		}
	}
	if *aggregate != "" {
		raw, err := json.MarshalIndent(perfreport.Aggregate(in.Head), "", "  ")
		if err == nil {
			err = os.WriteFile(*aggregate, raw, 0o600)
		}
		if err != nil {
			failf("aggregate: %v", err)
		}
	}
	v := perfreport.Evaluate(in)
	fmt.Print(perfreport.Markdown(v))
	switch {
	case len(v.Failures) > 0:
		os.Exit(1)
	case len(v.TimingFailures) > 0:
		os.Exit(3)
	}
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "perfreport: "+format+"\n", args...)
	os.Exit(2)
}

func load(pattern string) ([]perfreport.Report, error) {
	if pattern == "" {
		return nil, nil
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	reports := make([]perfreport.Report, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- paths come from the CI job's own glob
		if err != nil {
			return nil, err
		}
		var r perfreport.Report
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		reports = append(reports, r)
	}
	return reports, nil
}
