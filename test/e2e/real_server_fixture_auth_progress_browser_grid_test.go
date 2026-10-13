package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRealServerFixtureAuthProgressBrowserReportRequiresExactUpgradeGrid(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	valid := fixtureAuthProgressBrowserPassingJSON(t, identity)
	if _, err := decodeFixtureAuthProgressBrowserReport(valid, identity, fixtureAuthProgressBrowserTestManifestDigest()); err != nil {
		t.Errorf("rejected current runner's complete grid: %v", err)
	}
	type gridMutation struct {
		name   string
		mutate func(map[string]any)
	}
	mutations := make([]gridMutation, 0, 77)
	mutations = append(mutations, []gridMutation{
		{"missing grid", func(root map[string]any) {
			delete(fixtureAuthProgressBrowserJSONObject(t, root, "network"), "websocket_101_by_context_phase")
		}},
		{"case variant grid", func(root map[string]any) {
			network := fixtureAuthProgressBrowserJSONObject(t, root, "network")
			network["Websocket_101_by_context_phase"] = network["websocket_101_by_context_phase"]
			delete(network, "websocket_101_by_context_phase")
		}},
		{"runner supplied residual", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "network")["unattributed"] = 1
		}},
		{"null grid", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "network")["websocket_101_by_context_phase"] = nil
		}},
	}...)
	for _, context := range []string{"alice", "control", "other"} {
		mutations = append(mutations, gridMutation{"missing " + context, func(root map[string]any) {
			delete(fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase"), context)
		}}, gridMutation{"case variant " + context, func(root map[string]any) {
			grid := fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase")
			grid[strings.ToUpper(context[:1])+context[1:]] = grid[context]
			delete(grid, context)
		}})
		for _, phase := range []string{"pre_submit", "post_submit", "unknown"} {
			mutations = append(mutations, gridMutation{"missing " + context + "/" + phase, func(root map[string]any) {
				delete(fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase", context), phase)
			}}, gridMutation{"case variant " + context + "/" + phase, func(root map[string]any) {
				phases := fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase", context)
				phases[strings.ToUpper(phase)] = phases[phase]
				delete(phases, phase)
			}})
			for _, invalid := range []any{-1, int64(2147483648), nil, 0.5, "1"} {
				mutations = append(mutations, gridMutation{"invalid " + context + "/" + phase, func(root map[string]any) {
					fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase", context)[phase] = invalid
				}})
			}
		}
	}
	for _, extra := range []string{"bob", "Alice", "unattributed"} {
		mutations = append(mutations, gridMutation{"extra context " + extra, func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase")[extra] = map[string]any{"pre_submit": 0, "post_submit": 0, "unknown": 0}
		}})
	}
	mutations = append(mutations, gridMutation{"extra phase", func(root map[string]any) {
		fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase", "alice")["unattributed"] = 1
	}})
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			data := fixtureAuthProgressBrowserMutatedJSON(t, mutation.mutate)
			if _, err := decodeFixtureAuthProgressBrowserReport(data, identity, fixtureAuthProgressBrowserTestManifestDigest()); err == nil {
				t.Fatal("accepted malformed upgrade grid")
			}
		})
	}
	for _, context := range []string{"alice", "control", "other"} {
		for _, phase := range []string{"pre_submit", "post_submit", "unknown"} {
			data := fixtureAuthProgressBrowserMutatedJSON(t, func(root map[string]any) {
				fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase", context)[phase] = int64(2147483647)
			})
			if _, err := decodeFixtureAuthProgressBrowserReport(data, identity, fixtureAuthProgressBrowserTestManifestDigest()); err != nil {
				t.Errorf("rejected inclusive maximum at %s/%s: %v", context, phase, err)
			}
		}
	}
}

func TestRealServerFixtureAuthProgressBrowserReportRejectsLegacyRunnerProvenance(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	identity.SourceRefs.RunnerRevision = "877aeffca544728d415946d8e7ccb4aeae12df28"
	if _, err := decodeFixtureAuthProgressBrowserReport(fixtureAuthProgressBrowserPassingJSON(t, identity), identity, fixtureAuthProgressBrowserTestManifestDigest()); err == nil {
		t.Fatal("accepted retired runner provenance")
	}
	identity = fixtureAuthProgressBrowserTestIdentity()
	identity.SourceRefs.ArtifactSourceRevision = "097794ab18ef853ca09c3491b9df4e9a702d0ca6"
	if _, err := decodeFixtureAuthProgressBrowserReport(fixtureAuthProgressBrowserPassingJSON(t, identity), identity, fixtureAuthProgressBrowserTestManifestDigest()); err == nil {
		t.Fatal("accepted runner revision as artifact provenance")
	}
}

func TestRealServerFixtureAuthProgressBrowserReportRunnerCheckoutBinding(t *testing.T) {
	for _, testCase := range []struct {
		revision string
		want     bool
	}{
		{"097794ab18ef853ca09c3491b9df4e9a702d0ca6", true},
		{"877aeffca544728d415946d8e7ccb4aeae12df28", false},
	} {
		t.Run(testCase.revision, func(t *testing.T) {
			bin := t.TempDir()
			path := filepath.Join(bin, "git")
			script := "#!/bin/sh\ncase \"$*\" in\n *\"rev-parse HEAD\") printf '%s\\n' '" + testCase.revision + "' ;;\n *\"status --porcelain --untracked-files=all\") exit 0 ;;\n *) exit 2 ;;\nesac\n"
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // The temporary git stub exercises immutable checkout verification.
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			if _, err := verifiedFixtureAuthProgressRunnerCheckout(t.TempDir()); (err == nil) != testCase.want {
				t.Fatalf("checkout accepted=%t, want %t: %v", err == nil, testCase.want, err)
			}
		})
	}
}

func TestFixtureAuthProgressBrowserPairResultDerivesSeparateUnattributed(t *testing.T) {
	input := fixtureAuthProgressBrowserPairResultTestInput(t)
	root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(input))
	if root["schema_version"] != float64(2) || root["status"] != "available" {
		t.Fatalf("pair metadata = %v", root)
	}
	for _, leg := range []string{"tagged_browser", "untagged_browser"} {
		browser := fixtureAuthProgressBrowserJSONObject(t, root, leg)
		if browser["websocket_101_grid_stage"] != "available" {
			t.Fatalf("%s grid stage = %v", leg, browser["websocket_101_grid_stage"])
		}
		grid := fixtureAuthProgressBrowserJSONObject(t, browser, "websocket_101_by_context_phase")
		wantGrid := map[string]any{
			"alice":   map[string]any{"pre_submit": float64(1), "post_submit": float64(1), "unknown": float64(0)},
			"control": map[string]any{"pre_submit": float64(0), "post_submit": float64(0), "unknown": float64(0)},
			"other":   map[string]any{"pre_submit": float64(0), "post_submit": float64(0), "unknown": float64(0)},
		}
		if !reflect.DeepEqual(grid, wantGrid) {
			t.Errorf("%s grid = %v, want %v", leg, grid, wantGrid)
		}
		residual := fixtureAuthProgressBrowserJSONObject(t, browser, "unattributed")
		wantResidual := map[string]any{"available": false, "phase": "unknown"}
		if leg == "tagged_browser" {
			wantResidual = map[string]any{"available": true, "phase": "unknown", "count": float64(1)}
		}
		if !reflect.DeepEqual(residual, wantResidual) {
			t.Errorf("%s residual = %v, want %v", leg, residual, wantResidual)
		}
	}
}

func TestFixtureAuthProgressBrowserPairResultSumsAllCellsInInt64(t *testing.T) {
	input := fixtureAuthProgressBrowserPairResultTestInput(t)
	data := fixtureAuthProgressBrowserMutatedJSON(t, func(root map[string]any) {
		grid := fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase")
		for _, context := range []string{"alice", "control", "other"} {
			for _, phase := range []string{"pre_submit", "post_submit", "unknown"} {
				fixtureAuthProgressBrowserJSONObject(t, grid, context)[phase] = int64(2147483647)
			}
		}
	})
	report := fixtureAuthProgressBrowserDecodeTestJSON(t, data)
	input.TaggedBrowser = &report
	input.Tagged.authProgressResult.Window.Connections.WSAccepted = fixtureAuthProgressCounterDelta{
		Source: fixtureAuthProgressFrontSource, Before: 20, After: 19327352847, Delta: 19327352827,
	}
	root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(input))
	residual := fixtureAuthProgressBrowserJSONObject(t, root, "tagged_browser", "unattributed")
	if residual["count"] != float64(4) || residual["available"] != true {
		t.Fatalf("nine maximum cells residual = %v, want 4", residual)
	}
}

func TestFixtureAuthProgressBrowserPairResultRejectsUpgradeOvercount(t *testing.T) {
	input := fixtureAuthProgressBrowserPairResultTestInput(t)
	input.Tagged.authProgressResult.Window.Connections.WSAccepted = fixtureAuthProgressCounterDelta{
		Source: fixtureAuthProgressFrontSource, Before: 20, After: 21, Delta: 1,
	}
	root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(input))
	if root["status"] != "available" || root["unavailable_reason"] != nil {
		t.Fatalf("grid invalidity changed pair availability: %v", root)
	}
	fixtureAuthProgressBrowserAssertInvalidGrid(t, fixtureAuthProgressBrowserJSONObject(t, root, "tagged_browser"), "websocket_101_overcount")
	if fixtureAuthProgressBrowserJSONObject(t, root, "untagged_browser")["websocket_101_grid_stage"] != "available" {
		t.Fatal("tagged overcount discarded independent untagged grid")
	}
	if input.Tagged.authProgressResult.Window.Connections.WSAccepted.Delta != 1 {
		t.Fatal("overcount changed server counter")
	}
}

func TestFixtureAuthProgressBrowserPairResultValidatesEachLegGridUsability(t *testing.T) {
	type gridMutation struct {
		name   string
		stage  string
		mutate func(map[string]any)
	}
	mutations := make([]gridMutation, 0, 14+len(fixtureAuthProgressBrowserControlNames))
	mutations = append(mutations, []gridMutation{
		{"missing observation", "worker_observation_missing", func(root map[string]any) { root["workerObservation"] = nil }},
		{"unclassified observation", "worker_observation_unclassified", func(root map[string]any) {
			root["workerObservation"] = map[string]any{"classification": "unclassified"}
		}},
		{"aggregate overflow", "observer_overflow", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation")["observerOverflow"] = 1
		}},
		{"context overflow", "observer_overflow", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation", "contexts", "alice")["observerOverflow"] = 1
		}},
		{"network errors", "observer_errors", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "network")["observerErrors"] = 1
		}},
		{"worker errors", "observer_errors", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation")["observerErrors"] = 1
		}},
		{"incomplete controls", "observer_setup_incomplete", func(root map[string]any) {
			root["contextCount"], root["usernameSubmissions"], root["controlContextCount"] = 0, 0, 5
			root["passwordEvidence"] = []any{}
			root["stage"] = "alice_context"
		}},
	}...)
	for _, control := range fixtureAuthProgressBrowserControlNames {
		mutations = append(mutations, gridMutation{control + " overflow", "observer_overflow", func(root map[string]any) {
			fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation", "controls", control)["observerOverflow"] = 1
		}})
	}
	for _, stage := range []string{"runtime_inputs", "runtime_input_validation", "runtime_credential_validation", "browser_launch", "browser_worker_discovery", "observer_controls", "observer_control_mismatch"} {
		mutations = append(mutations, gridMutation{stage, "observer_setup_incomplete", func(root map[string]any) {
			root["stage"] = stage
			root["passwordEvidence"] = []any{}
		}})
	}
	for _, leg := range []string{"tagged_browser", "untagged_browser"} {
		for _, mutation := range mutations {
			t.Run(leg+"/"+mutation.name, func(t *testing.T) {
				input := fixtureAuthProgressBrowserPairResultTestInput(t)
				data := fixtureAuthProgressBrowserMutatedJSON(t, func(root map[string]any) {
					root["status"] = "failed"
					root["stage"] = "alice_password_card_not_reached"
					root["passwordEvidence"] = []any{map[string]any{"stage": "alice_password_card_not_reached", "count": 0}}
					grid := fixtureAuthProgressBrowserJSONObject(t, root, "network", "websocket_101_by_context_phase")
					for _, context := range []string{"alice", "control", "other"} {
						for _, phase := range []string{"pre_submit", "post_submit", "unknown"} {
							fixtureAuthProgressBrowserJSONObject(t, grid, context)[phase] = 0
						}
					}
					mutation.mutate(root)
				})
				report := fixtureAuthProgressBrowserDecodeTestJSON(t, data)
				if leg == "tagged_browser" {
					input.TaggedBrowser = &report
					if report.UsernameSubmissions == 0 {
						input.Tagged.authProgressResult.Window.Methods.AuthSendCode = fixtureAuthProgressMethodDelta{State: "not_reached"}
						input.Tagged.authProgressResult.Window.Methods.AuthSignIn = fixtureAuthProgressMethodDelta{State: "not_reached"}
					}
				} else {
					input.UntaggedBrowser = &report
				}
				root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(input))
				if root["status"] != "available" || root["unavailable_reason"] != nil {
					t.Fatalf("grid stage escaped into pair unavailability: %v", root)
				}
				browser := fixtureAuthProgressBrowserJSONObject(t, root, leg)
				fixtureAuthProgressBrowserAssertInvalidGrid(t, browser, mutation.stage)
				worker := fixtureAuthProgressBrowserJSONObject(t, browser, "worker_observation")
				wantState := "classified"
				switch mutation.name {
				case "missing observation":
					wantState = "missing"
				case "unclassified observation":
					wantState = "unclassified"
				}
				if worker["state"] != wantState {
					t.Errorf("worker state = %v, want %s", worker["state"], wantState)
				}
				if wantState != "classified" && (worker["observation"] != nil || browser["alice_target_coverage_complete"] != nil) {
					t.Fatal("missing or unclassified evidence fabricated counts or coverage")
				}
				otherLeg := "tagged_browser"
				if leg == otherLeg {
					otherLeg = "untagged_browser"
				}
				if fixtureAuthProgressBrowserJSONObject(t, root, otherLeg)["websocket_101_grid_stage"] != "available" {
					t.Fatal("invalid grid discarded other leg's usable evidence")
				}
			})
		}
	}
}

func TestFixtureAuthProgressBrowserPairResultRetainsFailedCardEvidenceAndTypedCounts(t *testing.T) {
	for _, incomplete := range []string{"", "coverageComplete", "discoveryActive", "missing alice"} {
		t.Run(incomplete, func(t *testing.T) {
			input := fixtureAuthProgressBrowserPairResultTestInput(t)
			data := fixtureAuthProgressBrowserMutatedJSON(t, func(root map[string]any) {
				root["status"], root["stage"] = "failed", "alice_password_card_not_reached"
				root["passwordEvidence"] = []any{map[string]any{"stage": "alice_password_card_not_reached", "count": 0}}
				if incomplete == "missing alice" {
					delete(fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation", "contexts"), "alice")
				} else if incomplete != "" {
					fixtureAuthProgressBrowserJSONObject(t, root, "workerObservation", "contexts", "alice")[incomplete] = 0
				}
			})
			report := fixtureAuthProgressBrowserDecodeTestJSON(t, data)
			input.TaggedBrowser = &report
			root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(input))
			browser := fixtureAuthProgressBrowserJSONObject(t, root, "tagged_browser")
			if browser["stage"] != "alice_password_card_not_reached" || browser["websocket_101_grid_stage"] != "available" ||
				fixtureAuthProgressBrowserJSONObject(t, browser, "unattributed")["count"] != float64(1) {
				t.Fatalf("usable failed-card evidence was discarded: %v", browser)
			}
			if browser["alice_target_coverage_complete"] != (incomplete == "") {
				t.Errorf("complete target coverage = %v", browser["alice_target_coverage_complete"])
			}
			network := fixtureAuthProgressBrowserJSONObject(t, browser, "alice_network")
			if fixtureAuthProgressBrowserJSONObject(t, network, "attemptsBySource")["page"] != float64(1) ||
				fixtureAuthProgressBrowserJSONObject(t, network, "allowedWebSocketsBySource")["page"] != float64(1) ||
				fixtureAuthProgressBrowserJSONObject(t, network, "unexpectedAttemptsBySource")["worker"] != float64(0) ||
				fixtureAuthProgressBrowserJSONObject(t, network, "workerTargets")["shared_worker"] != float64(1) {
				t.Fatalf("summary lost validated per-source/target counts: %v", network)
			}
			worker := fixtureAuthProgressBrowserJSONObject(t, browser, "worker_observation")
			if worker["state"] != "classified" || fixtureAuthProgressBrowserJSONObject(t, worker, "observation")["attachedTargets"] != float64(1) {
				t.Fatalf("summary lost classified worker counts: %v", worker)
			}
		})
	}
}

func TestFixtureAuthProgressBrowserPairResultUnavailableServerCannotEmitResidual(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*fixtureAuthProgressBrowserPairResultInput)
	}{
		{"absent result", func(input *fixtureAuthProgressBrowserPairResultInput) { input.Tagged.authProgressResult = nil }},
		{"absent window", func(input *fixtureAuthProgressBrowserPairResultInput) { input.Tagged.authProgressResult.Window = nil }},
		{"invalid identity", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.Tagged.authProgressResult.Identity.RunID = "invalid"
		}},
		{"retired server runner", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.Tagged.authProgressResult.Identity.SourceRefs.RunnerRevision = "877aeffca544728d415946d8e7ccb4aeae12df28"
		}},
		{"retired checkout provenance", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.RunnerRevision = "877aeffca544728d415946d8e7ccb4aeae12df28"
		}},
		{"missing checkout provenance", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.RunnerRevision = ""
		}},
		{"reset counter", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.Tagged.authProgressResult.Available = false
			input.Tagged.authProgressResult.UnavailableReason = "unavailable_counter_reset"
			input.Tagged.authProgressResult.Window = nil
		}},
		{"overflow counter", func(input *fixtureAuthProgressBrowserPairResultInput) {
			input.Tagged.authProgressResult.Available = false
			input.Tagged.authProgressResult.UnavailableReason = "unavailable_counter_overflow"
			input.Tagged.authProgressResult.Window = nil
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := fixtureAuthProgressBrowserPairResultTestInput(t)
			testCase.mutate(&input)
			got := fixtureAuthProgressBrowserPairResultFor(input)
			if got.Status != "unavailable" || got.TaggedBrowser != nil || got.UntaggedBrowser != nil || got.Server != nil {
				t.Fatalf("unvalidated server evidence emitted browser counts or residual: %+v", got)
			}
		})
	}
}

func TestFixtureAuthProgressBrowserPairResultSummaryEncodingUsesOnlyTypedFields(t *testing.T) {
	seen := make(map[reflect.Type]bool)
	var check func(reflect.Type)
	check = func(schema reflect.Type) {
		if seen[schema] {
			return
		}
		seen[schema] = true
		if schema == reflect.TypeFor[json.RawMessage]() || schema == reflect.TypeFor[[]byte]() || schema.Kind() == reflect.Interface {
			t.Fatalf("summary schema can pass through raw report data: %v", schema)
		}
		switch schema.Kind() {
		case reflect.Struct:
			for field := range schema.Fields() {
				check(field.Type)
			}
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Array:
			check(schema.Elem())
		}
	}
	check(reflect.TypeFor[fixtureAuthProgressBrowserPairOutput]())
	root := fixtureAuthProgressBrowserEncodedObject(t, fixtureAuthProgressBrowserPairResultFor(fixtureAuthProgressBrowserPairResultTestInput(t)))
	browser := fixtureAuthProgressBrowserJSONObject(t, root, "tagged_browser")
	keys := make([]string, 0, len(browser))
	for key := range browser {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"alice_allowed_websockets", "alice_network", "alice_target_coverage_complete", "context_count", "control_context_count", "manifest_artifact_digest", "observer_errors", "password_field_count", "password_fills", "password_submissions", "stage", "status", "unattributed", "unexpected_attempts", "username_submissions", "websocket_101_by_context_phase", "websocket_101_grid_stage", "worker_observation"}
	if !slices.Equal(keys, want) {
		t.Fatalf("summary keys = %v, want closed schema %v", keys, want)
	}
}

func fixtureAuthProgressBrowserAssertInvalidGrid(t *testing.T, summary map[string]any, stage string) {
	t.Helper()
	if summary["websocket_101_grid_stage"] != stage {
		t.Errorf("grid stage = %v, want %s", summary["websocket_101_grid_stage"], stage)
	}
	if _, ok := summary["websocket_101_by_context_phase"]; ok {
		t.Error("invalid grid emitted cells")
	}
	if _, ok := summary["unattributed"]; ok {
		t.Error("invalid grid emitted residual field")
	}
}

func fixtureAuthProgressBrowserMutatedJSON(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(fixtureAuthProgressBrowserPassingJSON(t, fixtureAuthProgressBrowserTestIdentity()), &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureAuthProgressBrowserDecodeTestJSON(t *testing.T, data []byte) fixtureAuthProgressBrowserReport {
	t.Helper()
	report, err := decodeFixtureAuthProgressBrowserReport(data, fixtureAuthProgressBrowserTestIdentity(), fixtureAuthProgressBrowserTestManifestDigest())
	if err != nil {
		t.Fatalf("decode structurally valid runner evidence: %v", err)
	}
	return report
}

func fixtureAuthProgressBrowserEncodedObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func fixtureAuthProgressBrowserJSONObject(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, key := range path {
		value, ok := object[key].(map[string]any)
		if !ok {
			t.Fatalf("JSON field %s is not an object: %v", key, object[key])
		}
		object = value
	}
	return object
}
