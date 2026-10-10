package e2e_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/gotd/td/telegram"
)

const dialogFilterPairStateCapacity = 256

type dialogFilterPairState struct {
	sequence uint64
	state    telegram.ConnectionState
}

type dialogFilterPairObserver struct {
	mu       sync.Mutex
	changed  *sync.Cond
	states   [dialogFilterPairStateCapacity]dialogFilterPairState
	count    int
	sequence uint64
	overflow uint64
}

func newDialogFilterPairObserver() *dialogFilterPairObserver {
	observer := &dialogFilterPairObserver{}
	observer.changed = sync.NewCond(&observer.mu)
	return observer
}

func (o *dialogFilterPairObserver) observe(state telegram.ConnectionState) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.sequence++
	if o.count == len(o.states) {
		o.overflow++
	} else {
		o.states[o.count] = dialogFilterPairState{sequence: o.sequence, state: state}
		o.count++
	}
	o.changed.Broadcast()
}

func (o *dialogFilterPairObserver) sequenceSnapshot() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sequence
}

func (o *dialogFilterPairObserver) snapshot() ([]dialogFilterPairState, uint64, uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	states := make([]dialogFilterPairState, o.count)
	copy(states, o.states[:o.count])
	return states, o.sequence, o.overflow
}

func dialogFilterPairReplacementReady(states []dialogFilterPairState, restartMark uint64) (uint64, bool) {
	sawDisconnected := false
	for _, event := range states {
		if event.sequence <= restartMark {
			continue
		}
		if event.state == telegram.ConnectionStateDisconnected {
			sawDisconnected = true
			continue
		}
		if sawDisconnected && event.state == telegram.ConnectionStateReady {
			return event.sequence, true
		}
	}
	return 0, false
}

func (o *dialogFilterPairObserver) waitForReplacementReady(ctx context.Context, restartMark uint64) (uint64, bool) {
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			o.mu.Lock()
			o.changed.Broadcast()
			o.mu.Unlock()
		case <-stopWatcher:
		}
	}()
	defer func() {
		close(stopWatcher)
		<-watcherDone
	}()

	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if sequence, ok := dialogFilterPairReplacementReady(o.states[:o.count], restartMark); ok {
			return sequence, true
		}
		if o.overflow > 0 || ctx.Err() != nil {
			return 0, false
		}
		o.changed.Wait()
	}
}

type dialogFilterPairStateRecord struct {
	Sequence uint64 `json:"seq"`
	State    string `json:"state"`
}

type dialogFilterPairRecord struct {
	Schema              int                           `json:"schema"`
	Variant             string                        `json:"variant"`
	States              []dialogFilterPairStateRecord `json:"states"`
	RestartMark         uint64                        `json:"restartMark"`
	ReadIssueSeq        *uint64                       `json:"readIssueSeq"`
	ReadReturnSeq       *uint64                       `json:"readReturnSeq"`
	ReplacementReadySeq *uint64                       `json:"replacementReadySeq"`
	Overflow            uint64                        `json:"overflow"`
	AcceptCount         uint64                        `json:"acceptCount"`
	Assertions          []string                      `json:"assertions"`
	SafeErrorClass      *string                       `json:"safeErrorClass"`
	Outcome             string                        `json:"outcome"`
	Authorized          *bool                         `json:"authorized,omitempty"`
}

func newDialogFilterPairRecord(
	observer *dialogFilterPairObserver,
	variant string,
	restartMark uint64,
	acceptCount uint64,
	readIssueSequence *uint64,
	readReturnSequence *uint64,
	authorized *bool,
	assertions []string,
	outcome string,
	err error,
) dialogFilterPairRecord {
	states, _, overflow := observer.snapshot()
	recordedStates := make([]dialogFilterPairStateRecord, len(states))
	for i, state := range states {
		recordedStates[i] = dialogFilterPairStateRecord{Sequence: state.sequence, State: state.state.String()}
	}
	var replacementReadySequence *uint64
	if sequence, ok := dialogFilterPairReplacementReady(states, restartMark); ok {
		replacementReadySequence = &sequence
	}
	var safeError *string
	if err != nil {
		classification := safeErrorClass(err)
		safeError = &classification
	}
	return dialogFilterPairRecord{
		Schema:              1,
		Variant:             variant,
		States:              recordedStates,
		RestartMark:         restartMark,
		ReadIssueSeq:        readIssueSequence,
		ReadReturnSeq:       readReturnSequence,
		ReplacementReadySeq: replacementReadySequence,
		Overflow:            overflow,
		AcceptCount:         acceptCount,
		Assertions:          append([]string(nil), assertions...),
		SafeErrorClass:      safeError,
		Outcome:             outcome,
		Authorized:          authorized,
	}
}

func logDialogFilterPairRecord(t *testing.T, record dialogFilterPairRecord) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal("[assert:dialog-filters.pair-observer-order] observer record could not be encoded")
	}
	t.Logf("PAIR_OBSERVER %s", encoded)
}
