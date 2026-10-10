package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
)

type reconnectAddrTuple struct {
	network string
	address string
}

type reconnectObservationEvent struct {
	seq          uint64
	kind         string
	ordinal      uint64
	class        string
	state        string
	gateArmed    bool
	tuple        reconnectAddrTuple
	tuplePresent bool
	at           time.Time
}

type reconnectClassificationInput struct {
	branch                 string
	cause                  string
	parentLive             bool
	ownDeadlineEarliest    bool
	windowMS               int64
	hasCloseMark           bool
	hasResolve             bool
	closeMark              uint64
	resolveSeq             uint64
	overflow               int
	correlationAmbiguous   bool
	closedMatchedA1Current int
	closeTotal             int
	closedAlready          int
	clientOpenAtClose      int
	registryAtClose        int
	registryAtResolve      int
	closeAt                time.Time
	late                   int
	events                 []reconnectObservationEvent
}

type reconnectClassification struct {
	category             string
	reason               string
	deadlineComplete     bool
	disconnectSeen       bool
	postMarkAccepts      int
	matchedAcceptOrdinal uint64
	correlationAmbiguous bool
	acceptErrors         [7]int
	events               []reconnectObservationEvent
	attempts             []reconnectAttemptClassification
	late                 int
}

type reconnectAttemptClassification struct {
	ordinal uint64
	state   string
	class   string
}

func classifyReconnectObservation(input reconnectClassificationInput) reconnectClassification {
	result := reconnectClassification{
		category: "inconclusive", reason: "incomplete", late: input.late,
		correlationAmbiguous: input.correlationAmbiguous,
	}
	if !input.hasCloseMark || !input.hasResolve || input.resolveSeq <= input.closeMark {
		result.reason = "missing_mark"
		return result
	}
	result.deadlineComplete = input.branch == "ctx_done" && input.cause == "deadline" && input.parentLive && input.ownDeadlineEarliest && input.windowMS >= 30000
	lateEvents := 0
	for _, event := range input.events {
		if event.seq > input.resolveSeq {
			lateEvents++
		}
	}
	if lateEvents > result.late {
		result.late = lateEvents
	}

	attempts := make(map[uint64]*reconnectAttemptClassification)
	var gateAccepts []reconnectObservationEvent
	dialTuples := make(map[reconnectAddrTuple]uint64)
	acceptTuples := make(map[reconnectAddrTuple]struct{})
	boundaryAttempt := false
	ambiguous := input.correlationAmbiguous
	for _, event := range input.events {
		if event.seq > input.resolveSeq {
			continue
		}
		if event.seq <= input.closeMark {
			continue
		}
		result.events = append(result.events, event)
		switch event.kind {
		case "dial_start":
			if _, exists := attempts[event.ordinal]; exists {
				ambiguous = true
				continue
			}
			attempts[event.ordinal] = &reconnectAttemptClassification{ordinal: event.ordinal, state: "pending"}
		case "dial_ok":
			attempt, ok := attempts[event.ordinal]
			if !ok {
				boundaryAttempt = true
				continue
			}
			if attempt.state != "pending" || !event.tuplePresent {
				ambiguous = true
				continue
			}
			attempt.state = "ok"
			if previous, exists := dialTuples[event.tuple]; exists && previous != event.ordinal {
				ambiguous = true
			}
			dialTuples[event.tuple] = event.ordinal
		case "dial_err":
			attempt, ok := attempts[event.ordinal]
			if !ok {
				boundaryAttempt = true
				continue
			}
			if attempt.state != "pending" {
				ambiguous = true
				continue
			}
			attempt.state = "err"
			attempt.class = fixedReconnectDialErrorClass(event.class)
		case "state":
			if event.state == "disconnected" {
				result.disconnectSeen = true
			}
		case "accept":
			result.postMarkAccepts++
			if event.gateArmed {
				gateAccepts = append(gateAccepts, event)
				if !event.tuplePresent {
					ambiguous = true
				} else if _, exists := acceptTuples[event.tuple]; exists {
					ambiguous = true
				} else {
					acceptTuples[event.tuple] = struct{}{}
				}
			}
		case "accept_err":
			index := reconnectAcceptErrorIndex(event.class)
			result.acceptErrors[index]++
		}
	}

	for _, attempt := range attempts {
		result.attempts = append(result.attempts, *attempt)
	}
	sort.Slice(result.attempts, func(i, j int) bool {
		return result.attempts[i].ordinal < result.attempts[j].ordinal
	})

	if len(gateAccepts) > 1 {
		ambiguous = true
	}
	result.correlationAmbiguous = ambiguous
	if len(gateAccepts) > 0 {
		for _, accepted := range gateAccepts {
			for _, attempt := range result.attempts {
				if attempt.state != "ok" {
					continue
				}
				dialTuple, ok := dialTuples[accepted.tuple]
				if accepted.tuplePresent && ok && dialTuple == attempt.ordinal {
					if result.matchedAcceptOrdinal != 0 && result.matchedAcceptOrdinal != attempt.ordinal {
						ambiguous = true
					}
					result.matchedAcceptOrdinal = attempt.ordinal
				}
			}
		}
	}
	result.correlationAmbiguous = ambiguous
	if input.overflow > 0 {
		result.reason = "overflow"
		return result
	}
	if ambiguous {
		result.reason = "ambiguous"
		return result
	}
	if boundaryAttempt {
		result.reason = "boundary_attempt"
		return result
	}

	if input.branch == "accepted" {
		if result.matchedAcceptOrdinal != 0 {
			result.category = "fixture_accept_matched"
			result.reason = ""
			return result
		}
		result.category = "accept_unmatched"
		result.reason = ""
		return result
	}
	if input.branch != "ctx_done" {
		result.reason = "branch"
		return result
	}
	if !result.deadlineComplete {
		result.category = "early_resolution"
		result.reason = ""
		return result
	}
	if len(gateAccepts) > 0 {
		result.reason = "branch_accept_observed"
		return result
	}
	if input.closedMatchedA1Current == 0 {
		result.category = "close_missed_a1"
		result.reason = ""
		return result
	}
	if len(result.attempts) == 0 {
		result.category = "missing_dial"
		result.reason = ""
		return result
	}

	last := result.attempts[len(result.attempts)-1]
	switch last.state {
	case "err":
		result.category = "dial_failed"
		result.reason = last.class
	case "pending":
		result.category = "dial_pending"
		result.reason = ""
	case "ok":
		if result.matchedAcceptOrdinal == last.ordinal {
			result.reason = "branch_accept_observed"
			return result
		}
		result.category = "dial_ok_no_accept"
		result.reason = ""
	default:
		result.reason = "attempt_state"
	}
	return result
}

func fixedReconnectDialErrorClass(class string) string {
	switch class {
	case "deadline", "canceled", "refused", "reset", "timeout", "closed_eof":
		return class
	default:
		return "other"
	}
}

func reconnectAcceptErrorIndex(class string) int {
	switch class {
	case "emfile":
		return 0
	case "enfile":
		return 1
	case "aborted":
		return 2
	case "reset":
		return 3
	case "timeout":
		return 4
	case "closed":
		return 5
	default:
		return 6
	}
}

const reconnectObservationCapacity = 512

type reconnectObservedAttempt struct {
	ordinal uint64
	state   string
	class   string
	open    bool
}

type reconnectObservation struct {
	mu sync.Mutex

	seq                 uint64
	events              []reconnectObservationEvent
	attempts            map[uint64]*reconnectObservedAttempt
	nextDialOrdinal     uint64
	openDialOrdinal     uint64
	dialTuples          map[reconnectAddrTuple]uint64
	gateAcceptTuples    map[reconnectAddrTuple]struct{}
	correlationAmbig    bool
	overflow            int
	late                int
	hasCloseMark        bool
	closeMark           uint64
	closeAt             time.Time
	closeTotal          int
	closedMatchedA1     int
	closedAlready       int
	clientOpenAtClose   int
	registryAtClose     int
	hasResolve          bool
	resolveSeq          uint64
	branch              string
	cause               string
	parentLive          bool
	ownDeadlineEarliest bool
	windowMS            int64
	registryAtResolve   int
}

func newReconnectObservation() *reconnectObservation {
	return &reconnectObservation{
		events:           make([]reconnectObservationEvent, 0, reconnectObservationCapacity),
		attempts:         make(map[uint64]*reconnectObservedAttempt),
		dialTuples:       make(map[reconnectAddrTuple]uint64),
		gateAcceptTuples: make(map[reconnectAddrTuple]struct{}),
	}
}

func (o *reconnectObservation) appendLocked(event reconnectObservationEvent) reconnectObservationEvent {
	o.seq++
	event.seq = o.seq
	event.at = time.Now()
	if o.hasResolve && event.seq > o.resolveSeq {
		o.late++
	}
	if len(o.events) == reconnectObservationCapacity {
		o.overflow++
		return event
	}
	o.events = append(o.events, event)
	return event
}

func (o *reconnectObservation) dial(ctx context.Context, network, address string) (net.Conn, error) {
	o.mu.Lock()
	o.nextDialOrdinal++
	ordinal := o.nextDialOrdinal
	o.appendLocked(reconnectObservationEvent{kind: "dial_start", ordinal: ordinal})
	o.attempts[ordinal] = &reconnectObservedAttempt{ordinal: ordinal, state: "started"}
	o.mu.Unlock()

	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		o.mu.Lock()
		o.appendLocked(reconnectObservationEvent{
			kind: "dial_err", ordinal: ordinal, class: reconnectDialErrorClass(err),
		})
		attempt := o.attempts[ordinal]
		attempt.state = "err"
		attempt.class = reconnectDialErrorClass(err)
		o.mu.Unlock()
		return conn, err
	}

	var tuple reconnectAddrTuple
	tuple, tuplePresent := reconnectTupleForAddr(conn.LocalAddr())
	o.mu.Lock()
	o.appendLocked(reconnectObservationEvent{
		kind: "dial_ok", ordinal: ordinal, tuple: tuple, tuplePresent: tuplePresent,
	})
	attempt := o.attempts[ordinal]
	attempt.state = "ok"
	attempt.open = true
	if !tuplePresent {
		o.correlationAmbig = true
	} else if previous, exists := o.dialTuples[tuple]; exists && previous != ordinal {
		o.correlationAmbig = true
	} else {
		o.dialTuples[tuple] = ordinal
	}
	if o.openDialOrdinal != 0 {
		if previous := o.attempts[o.openDialOrdinal]; previous != nil {
			previous.open = false
		}
		o.correlationAmbig = true
	}
	o.openDialOrdinal = ordinal
	o.mu.Unlock()
	return conn, nil
}

func reconnectDialErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case reconnectNetTimeout(err):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		return "closed_eof"
	default:
		return "other"
	}
}

func reconnectAcceptErrorClass(err error) string {
	switch {
	case errors.Is(err, syscall.EMFILE):
		return "emfile"
	case errors.Is(err, syscall.ENFILE):
		return "enfile"
	case errors.Is(err, syscall.ECONNABORTED):
		return "aborted"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case reconnectNetTimeout(err):
		return "timeout"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	default:
		return "other"
	}
}

func reconnectNetTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func reconnectTupleForAddr(addr net.Addr) (reconnectAddrTuple, bool) {
	if addr == nil {
		return reconnectAddrTuple{}, false
	}
	network, address := addr.Network(), addr.String()
	if network == "" || address == "" {
		return reconnectAddrTuple{}, false
	}
	return reconnectAddrTuple{network: network, address: address}, true
}

func reconnectContextCauseClass(cause error) string {
	switch {
	case cause == nil:
		return "other"
	case errors.Is(cause, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(cause, context.Canceled):
		return "canceled"
	default:
		if diagnostic, ok := errors.AsType[safeDiagnosticError](cause); ok && errors.Is(cause, diagnostic) {
			return "failure_signal"
		}
		return "other"
	}
}

func closeReconnectConnections(connections []net.Conn) (int, error) {
	closedAlready := 0
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			if errors.Is(err, net.ErrClosed) {
				closedAlready++
				continue
			}
			return closedAlready, err
		}
	}
	return closedAlready, nil
}

func (o *reconnectObservation) recordConnectionState(state telegram.ConnectionState) {
	stateName := "unknown"
	switch state {
	case telegram.ConnectionStateConnecting:
		stateName = "connecting"
	case telegram.ConnectionStateReady:
		stateName = "ready"
	case telegram.ConnectionStateDisconnected:
		stateName = "disconnected"
	}

	o.mu.Lock()
	o.appendLocked(reconnectObservationEvent{kind: "state", state: stateName})
	if state == telegram.ConnectionStateDisconnected && o.openDialOrdinal != 0 {
		if attempt := o.attempts[o.openDialOrdinal]; attempt != nil {
			attempt.open = false
		}
		o.openDialOrdinal = 0
	}
	o.mu.Unlock()
}

// recordAcceptLocked is called while the listener's existing mutex is held.
func (o *reconnectObservation) recordAcceptLocked(addr net.Addr, gateArmed bool) {
	tuple, tuplePresent := reconnectTupleForAddr(addr)
	o.mu.Lock()
	o.appendLocked(reconnectObservationEvent{
		kind: "accept", gateArmed: gateArmed, tuple: tuple, tuplePresent: tuplePresent,
	})
	if gateArmed {
		if !tuplePresent {
			o.correlationAmbig = true
		} else if _, exists := o.gateAcceptTuples[tuple]; exists {
			o.correlationAmbig = true
		} else {
			o.gateAcceptTuples[tuple] = struct{}{}
		}
	}
	o.mu.Unlock()
}

func (o *reconnectObservation) recordAcceptError(err error) {
	class := reconnectAcceptErrorClass(err)
	o.mu.Lock()
	o.appendLocked(reconnectObservationEvent{kind: "accept_err", class: class})
	o.mu.Unlock()
}

func (o *reconnectObservation) recordCloseMark(connections, activeAtClose []net.Conn, registryConns int) {
	o.mu.Lock()
	o.closeTotal = len(connections)
	o.registryAtClose = registryConns
	closing := make(map[net.Conn]struct{}, len(connections))
	for _, conn := range connections {
		closing[conn] = struct{}{}
	}
	closeTuples := make(map[reconnectAddrTuple]struct{}, len(connections))
	for _, conn := range activeAtClose {
		if _, willClose := closing[conn]; !willClose {
			continue
		}
		tuple, present := reconnectTupleForAddr(conn.RemoteAddr())
		if !present {
			o.correlationAmbig = true
			continue
		}
		if _, exists := closeTuples[tuple]; exists {
			o.correlationAmbig = true
		} else {
			closeTuples[tuple] = struct{}{}
		}
		if _, matched := o.dialTuples[tuple]; matched {
			o.closedMatchedA1++
		}
	}
	for _, attempt := range o.attempts {
		if attempt.open {
			o.clientOpenAtClose++
		}
	}
	mark := o.appendLocked(reconnectObservationEvent{kind: "close_mark"})
	o.hasCloseMark = true
	o.closeMark = mark.seq
	o.closeAt = mark.at
	o.mu.Unlock()
}

func (o *reconnectObservation) recordClosedAlready(count int) {
	o.mu.Lock()
	o.closedAlready = count
	o.mu.Unlock()
}

func (o *reconnectObservation) recordResolution(branch, cause string, parentLive, ownDeadlineEarliest bool, windowMS int64) {
	o.mu.Lock()
	resolution := o.appendLocked(reconnectObservationEvent{kind: "resolve"})
	o.hasResolve = true
	o.resolveSeq = resolution.seq
	o.branch = fixedReconnectBranch(branch)
	o.cause = fixedReconnectCause(cause)
	o.parentLive = parentLive
	o.ownDeadlineEarliest = ownDeadlineEarliest
	o.windowMS = windowMS
	o.mu.Unlock()
}

func fixedReconnectBranch(branch string) string {
	if branch == "accepted" || branch == "ctx_done" {
		return branch
	}
	return "other"
}

func fixedReconnectCause(cause string) string {
	switch cause {
	case "deadline", "canceled", "failure_signal":
		return cause
	default:
		return "other"
	}
}

func (o *reconnectObservation) recordRegistryAtResolve(registryConns int) {
	o.mu.Lock()
	o.registryAtResolve = registryConns
	o.mu.Unlock()
}

func (o *reconnectObservation) snapshot() reconnectClassificationInput {
	o.mu.Lock()
	defer o.mu.Unlock()
	events := append([]reconnectObservationEvent(nil), o.events...)
	return reconnectClassificationInput{
		branch:                 o.branch,
		cause:                  o.cause,
		parentLive:             o.parentLive,
		ownDeadlineEarliest:    o.ownDeadlineEarliest,
		windowMS:               o.windowMS,
		hasCloseMark:           o.hasCloseMark,
		hasResolve:             o.hasResolve,
		closeMark:              o.closeMark,
		resolveSeq:             o.resolveSeq,
		overflow:               o.overflow,
		correlationAmbiguous:   o.correlationAmbig,
		closedMatchedA1Current: o.closedMatchedA1,
		closeTotal:             o.closeTotal,
		closedAlready:          o.closedAlready,
		clientOpenAtClose:      o.clientOpenAtClose,
		registryAtClose:        o.registryAtClose,
		registryAtResolve:      o.registryAtResolve,
		closeAt:                o.closeAt,
		late:                   o.late,
		events:                 events,
	}
}

func (o *reconnectObservation) logSummary(t *testing.T) {
	t.Helper()
	input := o.snapshot()
	if !input.hasResolve {
		return
	}
	classification := classifyReconnectObservation(input)
	t.Log(o.formatSummary(input, classification))
}

func (o *reconnectObservation) formatSummary(input reconnectClassificationInput, classification reconnectClassification) string {
	var builder strings.Builder
	branch := fixedReconnectBranch(input.branch)
	cause := fixedReconnectCause(input.cause)
	reason := classification.reason
	if reason == "" {
		reason = "none"
	}
	fmt.Fprintf(&builder, "reconnect_observation client=A1 classification=%s reason=%s branch=%s", classification.category, reason, branch)
	fmt.Fprintf(&builder, " close_mark=%d resolve_seq=%d window_ms=%d own_deadline_earliest=%t parent_live=%t cause=%s deadline_complete=%t", input.closeMark, input.resolveSeq, input.windowMS, input.ownDeadlineEarliest, input.parentLive, cause, classification.deadlineComplete)
	fmt.Fprintf(&builder, " closed_total=%d closed_matched_a1_current=%d closed_already=%d a1_dialed_open=%d registry_close=%d registry_resolve=%d", input.closeTotal, input.closedMatchedA1Current, input.closedAlready, input.clientOpenAtClose, input.registryAtClose, input.registryAtResolve)
	fmt.Fprintf(&builder, " disconnect_seen=%t post_mark_accepts=%d", classification.disconnectSeen, classification.postMarkAccepts)
	fmt.Fprintf(&builder, " accept_err={emfile:%d,enfile:%d,aborted:%d,reset:%d,timeout:%d,closed:%d,other:%d}", classification.acceptErrors[0], classification.acceptErrors[1], classification.acceptErrors[2], classification.acceptErrors[3], classification.acceptErrors[4], classification.acceptErrors[5], classification.acceptErrors[6])
	fmt.Fprintf(&builder, " accept_matched_dial=%s", reconnectOrdinalValue(classification.matchedAcceptOrdinal))
	builder.WriteString(" attempts=[")
	for index, attempt := range classification.attempts {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, "%d:%s", attempt.ordinal, fixedReconnectAttemptState(attempt.state))
		if attempt.state == "err" {
			fmt.Fprintf(&builder, "(%s)", fixedReconnectDialErrorClass(attempt.class))
		}
	}
	builder.WriteByte(']')
	builder.WriteString(" events=[")
	for index, event := range classification.events {
		if index > 0 {
			builder.WriteByte(',')
		}
		o.formatEvent(&builder, event, input.closeAt)
	}
	builder.WriteByte(']')
	fmt.Fprintf(&builder, " late=%d overflow=%d correlation_ambiguous=%t", classification.late, input.overflow, classification.correlationAmbiguous)
	return builder.String()
}

func fixedReconnectAttemptState(state string) string {
	switch state {
	case "pending", "ok", "err":
		return state
	default:
		return "unknown"
	}
}

func reconnectOrdinalValue(ordinal uint64) string {
	if ordinal == 0 {
		return "none"
	}
	return strconv.FormatUint(ordinal, 10)
}

func (o *reconnectObservation) formatEvent(builder *strings.Builder, event reconnectObservationEvent, closeAt time.Time) {
	elapsedMS := event.at.Sub(closeAt).Milliseconds()
	switch event.kind {
	case "dial_start":
		fmt.Fprintf(builder, "dial_start#%d@%dms(n=%d)", event.seq, elapsedMS, event.ordinal)
	case "dial_ok":
		fmt.Fprintf(builder, "dial_ok#%d@%dms(n=%d)", event.seq, elapsedMS, event.ordinal)
	case "dial_err":
		fmt.Fprintf(builder, "dial_err#%d@%dms(n=%d,class=%s)", event.seq, elapsedMS, event.ordinal, fixedReconnectDialErrorClass(event.class))
	case "state":
		fmt.Fprintf(builder, "state#%d@%dms(%s)", event.seq, elapsedMS, fixedReconnectState(event.state))
	case "accept":
		fmt.Fprintf(builder, "accept#%d@%dms(gate=%t)", event.seq, elapsedMS, event.gateArmed)
	case "accept_err":
		fmt.Fprintf(builder, "accept_err#%d@%dms(class=%s)", event.seq, elapsedMS, fixedReconnectAcceptErrorClass(event.class))
	default:
		fmt.Fprintf(builder, "event#%d@%dms(unknown)", event.seq, elapsedMS)
	}
}

func fixedReconnectState(state string) string {
	switch state {
	case "connecting", "ready", "disconnected":
		return state
	default:
		return "unknown"
	}
}

func fixedReconnectAcceptErrorClass(class string) string {
	switch class {
	case "emfile", "enfile", "aborted", "reset", "timeout", "closed":
		return class
	default:
		return "other"
	}
}

func newReconnectObservedSmokeClient(t *testing.T, f *smokeFixture, label, phone string, observation *reconnectObservation) *smokeClient {
	t.Helper()
	sess := &session.StorageMemory{}
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	client := &smokeClient{
		client: telegram.NewClient(1, "hash", telegram.Options{
			DC:                f.dcID,
			DCList:            dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
			PublicKeys:        []telegram.PublicKey{{RSA: &f.key.PublicKey}},
			Resolver:          dcs.Plain(dcs.PlainOptions{Dial: observation.dial}),
			SessionStorage:    sess,
			UpdateHandler:     observedManagerHandler{observer: push, manager: manager},
			OnConnectionState: observation.recordConnectionState,
			Middlewares: []telegram.Middleware{
				hook.UpdateHook(manager.Handle),
				hook.AffectedHook(manager),
			},
		}),
		session: sess,
		manager: manager,
		seen:    seen,
		push:    push,
		cmds:    make(chan command),
		label:   label,
	}
	username := smokeAuthHandle(f, phone)
	flow := auth.NewFlow(
		auth.Constant(username, smokeUsernamePassword, auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			return f.codes.wait(ctx, username)
		})),
		auth.SendCodeOptions{},
	)
	ids, ready := make(chan int64, 1), make(chan struct{}, 1)
	client.lifecycle = startClientLifecycle(f.ctx, label, f.failures, func(phase *clientPhaseState) error {
		return runManagedInteractive(f.ctx, client.client, flow, ids, ready, client.cmds, manager, true, phase)
	})
	t.Cleanup(func() { client.stopClient(t) })
	loginStarted := time.Now()
	select {
	case client.id = <-ids:
	case <-f.ctx.Done():
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), contextFailureDescription(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), "cause="+safeErrorClass(client.lifecycle.result.error())))
	}
	managerStarted := time.Now()
	select {
	case <-ready:
	case <-f.ctx.Done():
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), contextFailureDescription(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), "cause="+safeErrorClass(client.lifecycle.result.error())))
	}
	return client
}

func TestReconnectObservationClassifierBoundaries(t *testing.T) {
	base := reconnectClassificationInput{
		branch:                 "ctx_done",
		cause:                  "deadline",
		parentLive:             true,
		ownDeadlineEarliest:    true,
		windowMS:               30000,
		hasCloseMark:           true,
		hasResolve:             true,
		closeMark:              2,
		resolveSeq:             7,
		closedMatchedA1Current: 1,
	}

	tests := []struct {
		name            string
		input           reconnectClassificationInput
		category        string
		reason          string
		late            int
		postMarkAccepts int
		acceptErrors    [7]int
	}{
		{
			name:     "deadline complete without dial is missing dial",
			input:    base,
			category: "missing_dial",
		},
		{
			name: "parent cancellation before own deadline is early resolution",
			input: func() reconnectClassificationInput {
				input := base
				input.cause = "canceled"
				input.parentLive = false
				input.windowMS = 12000
				return input
			}(),
			category: "early_resolution",
		},
		{
			name: "deadline window one millisecond short is early resolution",
			input: func() reconnectClassificationInput {
				input := base
				input.windowMS = 29999
				return input
			}(),
			category: "early_resolution",
		},
		{
			name: "parent deadline not later than accept deadline is early resolution",
			input: func() reconnectClassificationInput {
				input := base
				input.ownDeadlineEarliest = false
				return input
			}(),
			category: "early_resolution",
		},
		{
			name: "started final attempt without result is pending",
			input: func() reconnectClassificationInput {
				input := base
				input.events = []reconnectObservationEvent{{seq: 3, kind: "dial_start", ordinal: 1}}
				return input
			}(),
			category: "dial_pending",
		},
		{
			name: "unmatched gate accept is inconclusive",
			input: func() reconnectClassificationInput {
				input := base
				input.branch = "accepted"
				input.events = []reconnectObservationEvent{{
					seq: 3, kind: "accept", gateArmed: true,
					tuplePresent: true, tuple: reconnectAddrTuple{network: "tcp", address: "tuple-a"},
				}}
				return input
			}(),
			category:        "accept_unmatched",
			postMarkAccepts: 1,
		},
		{
			name: "events after resolve are late and excluded",
			input: func() reconnectClassificationInput {
				input := base
				input.events = []reconnectObservationEvent{{seq: 8, kind: "dial_start", ordinal: 1}}
				return input
			}(),
			category: "missing_dial",
			late:     1,
		},
		{
			name: "last failed dial retains its fixed class",
			input: func() reconnectClassificationInput {
				input := base
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "dial_start", ordinal: 1},
					{seq: 4, kind: "dial_err", ordinal: 1, class: "refused"},
				}
				return input
			}(),
			category: "dial_failed",
			reason:   "refused",
		},
		{
			name: "successful dial reports fixed fixture accept errors",
			input: func() reconnectClassificationInput {
				input := base
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "dial_start", ordinal: 1},
					{seq: 4, kind: "dial_ok", ordinal: 1, tuplePresent: true, tuple: reconnectAddrTuple{network: "tcp", address: "tuple-a"}},
					{seq: 5, kind: "accept_err", class: "emfile"},
					{seq: 6, kind: "accept_err", class: "enfile"},
				}
				return input
			}(),
			category:     "dial_ok_no_accept",
			acceptErrors: [7]int{1, 1},
		},
		{
			name: "matched accepted branch remains inconclusive",
			input: func() reconnectClassificationInput {
				input := base
				input.branch = "accepted"
				tuple := reconnectAddrTuple{network: "tcp", address: "tuple-a"}
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "dial_start", ordinal: 1},
					{seq: 4, kind: "dial_ok", ordinal: 1, tuplePresent: true, tuple: tuple},
					{seq: 5, kind: "accept", gateArmed: true, tuplePresent: true, tuple: tuple},
				}
				return input
			}(),
			category:        "fixture_accept_matched",
			postMarkAccepts: 1,
		},
		{
			name: "duplicate tuple makes correlation ambiguous",
			input: func() reconnectClassificationInput {
				input := base
				tuple := reconnectAddrTuple{network: "tcp", address: "tuple-a"}
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "dial_start", ordinal: 1},
					{seq: 4, kind: "dial_ok", ordinal: 1, tuplePresent: true, tuple: tuple},
					{seq: 5, kind: "dial_start", ordinal: 2},
					{seq: 6, kind: "dial_ok", ordinal: 2, tuplePresent: true, tuple: tuple},
				}
				return input
			}(),
			category: "inconclusive",
			reason:   "ambiguous",
		},
		{
			name: "multiple gate accepts make correlation ambiguous",
			input: func() reconnectClassificationInput {
				input := base
				input.branch = "accepted"
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "accept", gateArmed: true, tuplePresent: true, tuple: reconnectAddrTuple{network: "tcp", address: "tuple-a"}},
					{seq: 4, kind: "accept", gateArmed: true, tuplePresent: true, tuple: reconnectAddrTuple{network: "tcp", address: "tuple-b"}},
				}
				return input
			}(),
			category:        "inconclusive",
			reason:          "ambiguous",
			postMarkAccepts: 2,
		},
		{
			name: "missing A1 local tuple makes correlation ambiguous",
			input: func() reconnectClassificationInput {
				input := base
				input.events = []reconnectObservationEvent{
					{seq: 3, kind: "dial_start", ordinal: 1},
					{seq: 4, kind: "dial_ok", ordinal: 1},
				}
				return input
			}(),
			category: "inconclusive",
			reason:   "ambiguous",
		},
		{
			name: "deadline complete without close match is close missed",
			input: func() reconnectClassificationInput {
				input := base
				input.closedMatchedA1Current = 0
				return input
			}(),
			category: "close_missed_a1",
		},
		{
			name: "overflow prevents causal classification",
			input: func() reconnectClassificationInput {
				input := base
				input.overflow = 1
				return input
			}(),
			category: "inconclusive",
			reason:   "overflow",
		},
		{
			name: "missing close mark prevents causal classification",
			input: func() reconnectClassificationInput {
				input := base
				input.hasCloseMark = false
				return input
			}(),
			category: "inconclusive",
			reason:   "missing_mark",
		},
		{
			name: "ambiguous correlation prevents causal classification",
			input: func() reconnectClassificationInput {
				input := base
				input.correlationAmbiguous = true
				return input
			}(),
			category: "inconclusive",
			reason:   "ambiguous",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyReconnectObservation(tt.input)
			if got.category != tt.category || got.reason != tt.reason || got.late != tt.late || got.postMarkAccepts != tt.postMarkAccepts || got.acceptErrors != tt.acceptErrors {
				t.Fatalf("classification = %s/%s late=%d post_mark_accepts=%d accept_errors=%v, want %s/%s late=%d post_mark_accepts=%d accept_errors=%v", got.category, got.reason, got.late, got.postMarkAccepts, got.acceptErrors, tt.category, tt.reason, tt.late, tt.postMarkAccepts, tt.acceptErrors)
			}
		})
	}
}
