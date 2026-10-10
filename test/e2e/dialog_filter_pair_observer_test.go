package e2e_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gotd/td/telegram"
)

func TestDialogFilterPairReplacementReadyRequiresPostMarkDisconnect(t *testing.T) {
	events := []dialogFilterPairState{
		{sequence: 1, state: telegram.ConnectionStateReady},
		{sequence: 2, state: telegram.ConnectionStateReady},
		{sequence: 3, state: telegram.ConnectionStateDisconnected},
		{sequence: 4, state: telegram.ConnectionStateConnecting},
		{sequence: 5, state: telegram.ConnectionStateDisconnected},
		{sequence: 6, state: telegram.ConnectionStateConnecting},
		{sequence: 7, state: telegram.ConnectionStateReady},
	}

	if _, ok := dialogFilterPairReplacementReady(events[:2], 1); ok {
		t.Fatal("Ready without a post-mark Disconnected qualified as replacement Ready")
	}

	readySequence, ok := dialogFilterPairReplacementReady(events, 1)
	if !ok || readySequence != 7 {
		t.Fatalf("replacement Ready = %d, %t; want sequence 7 after the post-mark reconnect cycles", readySequence, ok)
	}
}

func TestDialogFilterPairObserverBufferIsBounded(t *testing.T) {
	observer := newDialogFilterPairObserver()
	for range dialogFilterPairStateCapacity + 3 {
		observer.observe(telegram.ConnectionStateConnecting)
	}

	states, sequence, overflow := observer.snapshot()
	if len(states) != dialogFilterPairStateCapacity {
		t.Fatalf("recorded states = %d, want fixed capacity %d", len(states), dialogFilterPairStateCapacity)
	}
	if sequence != dialogFilterPairStateCapacity+3 || overflow != 3 {
		t.Fatalf("sequence/overflow = %d/%d, want %d/3", sequence, overflow, dialogFilterPairStateCapacity+3)
	}
}

func TestDialogFilterPairObserverRecordIsSanitizedAndVariantScoped(t *testing.T) {
	observer := newDialogFilterPairObserver()
	observer.observe(telegram.ConnectionStateReady)
	restartMark := observer.sequenceSnapshot()
	observer.observe(telegram.ConnectionStateDisconnected)
	observer.observe(telegram.ConnectionStateConnecting)
	observer.observe(telegram.ConnectionStateReady)
	issueSequence := observer.sequenceSnapshot()
	returnSequence := issueSequence

	immediateAssertions := []string{
		"dialog-filters.pair-immediate-read",
		"dialog-filters.pair-observer-overflow",
		"dialog-filters.pair-observer-order",
	}
	privateError := errors.New("PRIVATE_FILTER_PAYLOAD_SENTINEL")
	immediate := newDialogFilterPairRecord(
		observer,
		"immediate",
		restartMark,
		4,
		&issueSequence,
		&returnSequence,
		nil,
		immediateAssertions,
		"failure",
		privateError,
	)
	encodedImmediate, err := json.Marshal(immediate)
	if err != nil {
		t.Fatalf("marshal immediate observer record: %v", err)
	}
	if strings.Contains(string(encodedImmediate), "PRIVATE_FILTER_PAYLOAD_SENTINEL") {
		t.Fatal("observer record exposed the raw failure message")
	}
	var immediateFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedImmediate, &immediateFields); err != nil {
		t.Fatalf("decode immediate observer record: %v", err)
	}
	if _, ok := immediateFields["authorized"]; ok {
		t.Fatal("immediate observer record included gated authorization state")
	}
	wantImmediateFields := [...]string{
		"schema", "variant", "states", "restartMark", "readIssueSeq", "readReturnSeq",
		"replacementReadySeq", "overflow", "acceptCount", "assertions", "safeErrorClass", "outcome",
	}
	if len(immediateFields) != len(wantImmediateFields) {
		t.Fatalf("immediate observer fields = %d, want %d allowlisted fields", len(immediateFields), len(wantImmediateFields))
	}
	for _, field := range wantImmediateFields {
		if _, ok := immediateFields[field]; !ok {
			t.Fatalf("immediate observer record omitted allowlisted field %q", field)
		}
	}
	var states []struct {
		Sequence uint64 `json:"seq"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(immediateFields["states"], &states); err != nil {
		t.Fatalf("decode observer states: %v", err)
	}
	wantStates := []struct {
		sequence uint64
		state    string
	}{{1, "ready"}, {2, "disconnected"}, {3, "connecting"}, {4, "ready"}}
	if len(states) != len(wantStates) {
		t.Fatalf("recorded states = %d, want %d", len(states), len(wantStates))
	}
	for i, want := range wantStates {
		if states[i].Sequence != want.sequence || states[i].State != want.state {
			t.Fatalf("recorded state %d = %d:%s, want %d:%s", i, states[i].Sequence, states[i].State, want.sequence, want.state)
		}
	}
	var safeError string
	if err := json.Unmarshal(immediateFields["safeErrorClass"], &safeError); err != nil {
		t.Fatalf("decode safe error class: %v", err)
	}
	if !strings.HasPrefix(safeError, "other(") || strings.Contains(safeError, "PRIVATE_FILTER_PAYLOAD_SENTINEL") {
		t.Fatalf("safe error class = %q, want a sanitized Go error type", safeError)
	}

	authorized := true
	gatedAssertions := []string{
		"dialog-filters.pair-gated-replacement-ready",
		"dialog-filters.pair-gated-auth-status",
		"dialog-filters.pair-gated-read",
		"dialog-filters.pair-observer-overflow",
		"dialog-filters.pair-observer-order",
	}
	gated := newDialogFilterPairRecord(
		observer,
		"gated",
		restartMark,
		4,
		&issueSequence,
		&returnSequence,
		&authorized,
		gatedAssertions,
		"pass",
		nil,
	)
	encodedGated, err := json.Marshal(gated)
	if err != nil {
		t.Fatalf("marshal gated observer record: %v", err)
	}
	var gatedFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedGated, &gatedFields); err != nil {
		t.Fatalf("decode gated observer record: %v", err)
	}
	var wantGatedFields [len(wantImmediateFields) + 1]string
	copy(wantGatedFields[:], wantImmediateFields[:])
	wantGatedFields[len(wantImmediateFields)] = "authorized"
	if len(gatedFields) != len(wantGatedFields) || string(gatedFields["authorized"]) != "true" {
		t.Fatalf("gated observer authorization/field count = %s/%d, want true/%d", gatedFields["authorized"], len(gatedFields), len(wantGatedFields))
	}
	for _, field := range wantGatedFields {
		if _, ok := gatedFields[field]; !ok {
			t.Fatalf("gated observer record omitted allowlisted field %q", field)
		}
	}
}
