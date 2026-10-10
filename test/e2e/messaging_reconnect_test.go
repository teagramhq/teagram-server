package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestMessagingReconnectPushGap(t *testing.T) {
	t.Parallel()
	f := newSmokeFixtureWithDeadline(t, config.RegistrationClosed, nil, 3*time.Minute)
	reconnectObserver := newReconnectObserver()
	f.listener.setReconnectObserver(reconnectObserver)
	var closeEvidence reconnectCloseEvidence
	summaryReported := false
	t.Cleanup(func() {
		if !summaryReported {
			snapshot := reconnectObserver.metadataWithoutEvents()
			t.Log(formatReconnectSummary(snapshot, closeEvidence, false, false, reconnectInconclusive))
		}
	})

	const phoneA, phoneB, phoneC = "+15551046101", "+15551046102", "+15551046103"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)

	a1 := newReconnectObserverSmokeClient(t, f, "A1", phoneA, reconnectObserver)
	a1Connections := f.listener.activeConnections()
	if len(a1Connections) == 0 {
		t.Fatal("A1 has no accepted fixture connection")
	}
	var advertised *tg.Config
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		cfg, err := api.HelpGetConfig(ctx)
		advertised = cfg
		return err
	}); err != nil {
		t.Fatalf("A1 config request failed (cause=%s)", reconnectDialErrorClass(err))
	}
	advertisementMatches := fixtureConfigMatchesListener(advertised, f.dcID, f.listener)
	keysBeforeFirstReconnect, err := reconnectAuthKeyIDs(f.ctx, f.store, a1.id)
	if err != nil {
		t.Fatal("assertion=auth_key_snapshot_before_first_reconnect")
	}
	acceptsBeforeFirstReconnect := f.listener.acceptCount()
	releaseFirstAccept, firstAccepted := f.listener.pauseAccept()
	defer releaseFirstAccept()
	if err := f.listener.closeConnections(a1Connections); err != nil {
		t.Fatal("assertion=first_a1_socket_close")
	}
	firstReconnectCtx, cancelFirstReconnect := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancelFirstReconnect()
	select {
	case <-firstAccepted:
	case <-firstReconnectCtx.Done():
		t.Fatalf("assertion=first_a1_accept_select accepts_before=%d accepts_after=%d cause=%s", acceptsBeforeFirstReconnect, f.listener.acceptCount(), reconnectContextFailureClass(firstReconnectCtx))
	}
	if f.listener.acceptCount() <= acceptsBeforeFirstReconnect {
		t.Fatalf("A1 reconnect did not produce a fresh accept (before=%d after=%d)", acceptsBeforeFirstReconnect, f.listener.acceptCount())
	}
	if !advertisementMatches {
		t.Fatal("A1 config did not advertise its bound fixture listener")
	}
	releaseFirstAccept()
	waitForDistinctAuthKeys(t, firstReconnectCtx, f.registry, a1.id, 1, "A1", a1.lifecycle)
	if err := a1.call(firstReconnectCtx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("A1 resumed request failed (cause=%s)", reconnectDialErrorClass(err))
	}
	keysAfterFirstReconnect, err := reconnectAuthKeyIDs(f.ctx, f.store, a1.id)
	cancelFirstReconnect()
	if err != nil {
		t.Fatal("assertion=auth_key_verification_after_first_reconnect")
	}
	if !sameReconnectAuthKeyIDs(keysBeforeFirstReconnect, keysAfterFirstReconnect) {
		t.Fatal("A authenticated key set changed across reconnect")
	}
	a1Connections = f.listener.activeConnections()

	a2 := newSmokeClient(t, f, "A2", phoneA)
	b1 := newSmokeClient(t, f, "B1", phoneB)
	b2 := newSmokeClient(t, f, "B2", phoneB)
	c := newSmokeClient(t, f, "C", phoneC)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, a1.id, 2, "A1", a1.lifecycle)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, b1.id, 2, "B1", b1.lifecycle)

	keysBefore, err := reconnectAuthKeyIDs(f.ctx, f.store, a1.id)
	if err != nil {
		t.Fatal("assertion=auth_key_snapshot_before_second_reconnect")
	}
	acceptsBefore := f.listener.acceptCount()
	releaseAccept, accepted := f.listener.pauseAccept()
	defer releaseAccept()
	closeEvidence = reconnectObserver.markClose(a1Connections, len(f.registry.Conns(a1.id)))
	closedAlready, err := closeReconnectConnections(a1Connections)
	closeEvidence.closedAlready = closedAlready
	if err != nil {
		t.Fatal("assertion=second_a1_socket_close")
	}
	acceptCtx, cancelAccept := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancelAccept()
	acceptedInSelect := false
	select {
	case <-accepted:
		acceptedInSelect = true
	case <-acceptCtx.Done():
	}
	closeEvidence.registryAtResolve = len(f.registry.Conns(a1.id))
	closeEvidence.registryAtResolveRecorded = true
	reconnectSnapshot := reconnectObserver.snapshotAfterSelect()
	reconnectClass := classifyReconnectObservation(reconnectSnapshot, closeEvidence, acceptedInSelect)
	reconnectSummary := formatReconnectSummary(reconnectSnapshot, closeEvidence, true, acceptedInSelect, reconnectClass)
	t.Log(reconnectSummary)
	summaryReported = true
	if !acceptedInSelect {
		t.Fatalf("assertion=second_a1_accept_select accepts_before=%d accepts_after=%d", acceptsBefore, f.listener.acceptCount())
	}
	cancelAccept()
	if f.listener.acceptCount() <= acceptsBefore {
		t.Fatalf("A1 reconnect did not produce a fresh accept (before=%d after=%d)", acceptsBefore, f.listener.acceptCount())
	}
	if !advertisementMatches {
		t.Fatal("A1 config did not advertise its bound fixture listener")
	}

	const gapText = "A2 sent during A1 reconnect gap"
	var gapResult tg.UpdatesClass
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a2.id, b1.id), Message: gapText, RandomID: 1046101,
		})
		gapResult = res
		return err
	}); err != nil {
		t.Fatalf("A2 gap send failed (cause=%s)", reconnectDialErrorClass(err))
	}
	gapSenderMessage, gapSenderPts, ok := outgoingMessage(t, gapResult, gapText)
	if !ok || countOutgoingMessages(gapResult, gapText) != 1 {
		t.Fatal("A2 gap send omitted exactly one outgoing message")
	}
	assertReconnectMessage(t, f.ctx, a2.seen, gapText, gapSenderMessage.ID, true, b1.id, gapSenderPts, "A2 gap result")
	assertReconnectMessage(t, f.ctx, b1.push, gapText, reconnectHistoryMessageID(t, f, b1, a1.id, gapText), false, a1.id, reconnectStatePts(t, f, b1.id), "B1 gap push")
	assertReconnectMessage(t, f.ctx, b2.push, gapText, reconnectHistoryMessageID(t, f, b2, a1.id, gapText), false, a1.id, reconnectStatePts(t, f, b1.id), "B2 gap push")
	assertReconnectNoMessage(t, f.ctx, c.seen, "C during gap")
	assertReconnectNoMessage(t, f.ctx, c.push, "C push during gap")
	assertReconnectNoMessage(t, f.ctx, a2.push, "A2 origin push during gap")

	releaseAccept()
	resumeCtx, cancelResume := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancelResume()
	waitForDistinctAuthKeys(t, resumeCtx, f.registry, a1.id, 2, "A1", a1.lifecycle)
	var resumed *tg.UpdatesState
	if err := a1.call(resumeCtx, func(ctx context.Context, api *tg.Client) error {
		state, err := api.UpdatesGetState(ctx)
		resumed = state
		return err
	}); err != nil {
		t.Fatalf("A1 resumed request failed (cause=%s)", reconnectDialErrorClass(err))
	}
	var gapDifference tg.UpdatesDifferenceClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
		gapDifference = diff
		return err
	}); err != nil {
		t.Fatalf("A1 gap difference request failed (cause=%s)", safeReconnectCallFailure(a1, err))
	}
	fullDifference, ok := gapDifference.(*tg.UpdatesDifference)
	if !ok || fullDifference == nil {
		t.Fatal("assertion=a1_gap_difference_response_type")
	}
	if !reconnectDifferenceHasMessage(fullDifference, gapText, gapSenderMessage.ID, b1.id) || fullDifference.State.Pts != gapSenderPts {
		t.Fatalf("A1 gap difference mismatch (message count=%d pts=%d)", len(fullDifference.NewMessages), fullDifference.State.Pts)
	}
	keysAfter, err := reconnectAuthKeyIDs(f.ctx, f.store, a1.id)
	if err != nil {
		t.Fatal("assertion=auth_key_verification_after_second_reconnect")
	}
	if !sameReconnectAuthKeyIDs(keysBefore, keysAfter) {
		t.Fatal("A authenticated key set changed across reconnect")
	}
	accountState, err := f.store.State(f.ctx, a1.id)
	if err != nil {
		t.Fatal("assertion=a_state_verification")
	}
	if resumed == nil || resumed.Pts != accountState.Pts {
		t.Fatalf("A1 resumed state pts mismatch (got=%d want=%d)", statePts(resumed), accountState.Pts)
	}
	const liveText = "A1 sent after reconnect"
	var liveResult tg.UpdatesClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a1.id, b1.id), Message: liveText, RandomID: 1046102,
		})
		liveResult = res
		return err
	}); err != nil {
		t.Fatalf("A1 live send failed (cause=%s)", reconnectDialErrorClass(err))
	}
	liveSenderMessage, liveSenderPts, ok := outgoingMessage(t, liveResult, liveText)
	if !ok || countOutgoingMessages(liveResult, liveText) != 1 {
		t.Fatal("A1 live send omitted exactly one outgoing message")
	}
	assertReconnectMessage(t, f.ctx, a2.push, liveText, liveSenderMessage.ID, true, b1.id, liveSenderPts, "A2 live push")
	assertReconnectMessage(t, f.ctx, b1.push, liveText, reconnectHistoryMessageID(t, f, b1, a1.id, liveText), false, a1.id, reconnectStatePts(t, f, b1.id), "B1 live push")
	assertReconnectMessage(t, f.ctx, b2.push, liveText, reconnectHistoryMessageID(t, f, b2, a1.id, liveText), false, a1.id, reconnectStatePts(t, f, b1.id), "B2 live push")
	assertReconnectOriginSuppressed(t, f.ctx, a1.push, liveText, liveSenderMessage.ID, gapText, gapSenderMessage.ID, gapSenderPts, b1.id, "A1")
	assertReconnectNoMessage(t, f.ctx, c.seen, "C after reconnect")
	assertReconnectNoMessage(t, f.ctx, c.push, "C push after reconnect")
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
		if err != nil {
			return err
		}
		if empty, ok := diff.(*tg.UpdatesDifferenceEmpty); ok && empty != nil {
			return nil
		}
		if full, ok := diff.(*tg.UpdatesDifference); ok && len(full.NewMessages) == 0 && len(full.OtherUpdates) == 0 {
			return nil
		}
		return errors.New("C difference exposed A/B updates")
	}); err != nil {
		t.Fatalf("C isolation difference failed (cause=%s)", reconnectDialErrorClass(err))
	}
}

const reconnectObservationCapacity = 256

type reconnectEventKind uint8

const (
	reconnectStateEvent reconnectEventKind = iota
	reconnectDialStartEvent
	reconnectDialOKEvent
	reconnectDialErrorEvent
	reconnectAcceptEvent
)

type reconnectDialError uint8

const (
	reconnectDialErrorOther reconnectDialError = iota
	reconnectDialErrorDeadline
	reconnectDialErrorCanceled
	reconnectDialErrorRefused
	reconnectDialErrorReset
	reconnectDialErrorTimeout
	reconnectDialErrorClosedEOF
)

func (c reconnectDialError) String() string {
	switch c {
	case reconnectDialErrorDeadline:
		return "deadline"
	case reconnectDialErrorCanceled:
		return "canceled"
	case reconnectDialErrorRefused:
		return "refused"
	case reconnectDialErrorReset:
		return "reset"
	case reconnectDialErrorTimeout:
		return "timeout"
	case reconnectDialErrorClosedEOF:
		return "closed_eof"
	default:
		return "other"
	}
}

type reconnectEvent struct {
	seq        uint64
	kind       reconnectEventKind
	at         time.Time
	state      telegram.ConnectionState
	attempt    uint64
	errorClass reconnectDialError
	localAddr  net.Addr
	remoteAddr net.Addr
}

type reconnectDial struct {
	localAddr  net.Addr
	remoteAddr net.Addr
	open       bool
}

type reconnectObserver struct {
	mu            sync.Mutex
	events        []reconnectEvent
	seq           uint64
	overflow      uint64
	nextAttempt   uint64
	currentDial   reconnectDial
	closeMarked   bool
	closeMark     uint64
	closeMarkedAt time.Time
}

type reconnectObservationSnapshot struct {
	events      []reconnectEvent
	overflow    uint64
	closeMarked bool
	closeMark   uint64
	closeAt     time.Time
}

type reconnectCloseEvidence struct {
	markSet                   bool
	markSeq                   uint64
	closeTotal                int
	closedMatchedA1Current    int
	closedAlready             int
	clientOpen                int
	registryAtClose           int
	registryAtResolve         int
	registryAtResolveRecorded bool
	closeCorrelationComplete  bool
}

type reconnectClassification string

const (
	reconnectInconclusive             reconnectClassification = "inconclusive"
	reconnectCloseMissedA1            reconnectClassification = "close_missed_a1"
	reconnectMissingDial              reconnectClassification = "missing_dial"
	reconnectDialUnsuccessful         reconnectClassification = "unsuccessful_dial"
	reconnectDialWithoutFixtureAccept reconnectClassification = "successful_dial_without_fixture_accept"
	reconnectFixtureAcceptMatched     reconnectClassification = "fixture_accept_matched"
)

func newReconnectObserver() *reconnectObserver {
	return &reconnectObserver{
		events: make([]reconnectEvent, 0, reconnectObservationCapacity),
	}
}

func (o *reconnectObserver) appendLocked(event reconnectEvent) {
	o.seq++
	event.seq = o.seq
	event.at = time.Now()
	if len(o.events) == cap(o.events) {
		o.overflow++
		return
	}
	o.events = append(o.events, event)
}

func (o *reconnectObserver) connectionState(state telegram.ConnectionState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.appendLocked(reconnectEvent{kind: reconnectStateEvent, state: state})
	if state == telegram.ConnectionStateDisconnected {
		o.currentDial.open = false
	}
}

func (o *reconnectObserver) startDial() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextAttempt++
	ordinal := o.nextAttempt
	o.appendLocked(reconnectEvent{kind: reconnectDialStartEvent, attempt: ordinal})
	return ordinal
}

func (o *reconnectObserver) dial(ctx context.Context, network, address string) (net.Conn, error) {
	ordinal := o.startDial()
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		o.dialError(ordinal, reconnectDialErrorClass(err))
		return conn, err
	}
	if conn == nil {
		o.dialSucceeded(ordinal, nil, nil)
		return conn, err
	}
	o.dialSucceeded(ordinal, conn.LocalAddr(), conn.RemoteAddr())
	return conn, err
}

func (o *reconnectObserver) dialSucceeded(ordinal uint64, localAddr, remoteAddr net.Addr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.appendLocked(reconnectEvent{
		kind:       reconnectDialOKEvent,
		attempt:    ordinal,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	})
	o.currentDial = reconnectDial{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		open:       localAddr != nil && remoteAddr != nil,
	}
}

func (o *reconnectObserver) dialError(ordinal uint64, class reconnectDialError) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.appendLocked(reconnectEvent{kind: reconnectDialErrorEvent, attempt: ordinal, errorClass: class})
}

func (o *reconnectObserver) accept(localAddr, remoteAddr net.Addr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.appendLocked(reconnectEvent{
		kind:       reconnectAcceptEvent,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	})
}

func (o *reconnectObserver) markClose(connections []net.Conn, registryCount int) reconnectCloseEvidence {
	o.mu.Lock()
	defer o.mu.Unlock()

	evidence := reconnectCloseEvidence{
		closeTotal:               len(connections),
		clientOpen:               0,
		registryAtClose:          registryCount,
		closeCorrelationComplete: true,
	}
	if o.currentDial.open {
		evidence.clientOpen = 1
		if o.currentDial.localAddr == nil || o.currentDial.remoteAddr == nil {
			evidence.closeCorrelationComplete = false
		}
	}
	for _, conn := range connections {
		if conn == nil || conn.LocalAddr() == nil || conn.RemoteAddr() == nil {
			evidence.closeCorrelationComplete = false
			continue
		}
		if o.currentDial.open && sameReconnectEndpoints(
			o.currentDial.localAddr,
			o.currentDial.remoteAddr,
			conn.RemoteAddr(),
			conn.LocalAddr(),
		) {
			evidence.closedMatchedA1Current++
		}
	}
	evidence.markSet = true
	evidence.markSeq = o.seq
	markedAt := time.Now()
	o.closeMarked = true
	o.closeMark = evidence.markSeq
	o.closeMarkedAt = markedAt
	return evidence
}

func (o *reconnectObserver) snapshotAfterSelect() reconnectObservationSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return reconnectObservationSnapshot{
		events:      append([]reconnectEvent(nil), o.events...),
		overflow:    o.overflow,
		closeMarked: o.closeMarked,
		closeMark:   o.closeMark,
		closeAt:     o.closeMarkedAt,
	}
}

func (o *reconnectObserver) metadataWithoutEvents() reconnectObservationSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return reconnectObservationSnapshot{
		overflow:    o.overflow,
		closeMarked: o.closeMarked,
		closeMark:   o.closeMark,
		closeAt:     o.closeMarkedAt,
	}
}

func sameReconnectEndpoint(a, b net.Addr) bool {
	return a != nil && b != nil && a.Network() == b.Network() && a.String() == b.String()
}

func sameReconnectEndpoints(dialLocal, dialRemote, acceptRemote, acceptLocal net.Addr) bool {
	return sameReconnectEndpoint(dialLocal, acceptRemote) && sameReconnectEndpoint(dialRemote, acceptLocal)
}

func reconnectDialErrorClass(err error) reconnectDialError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return reconnectDialErrorDeadline
	case errors.Is(err, context.Canceled):
		return reconnectDialErrorCanceled
	case errors.Is(err, syscall.ECONNREFUSED):
		return reconnectDialErrorRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED):
		return reconnectDialErrorReset
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.Is(err, os.ErrClosed):
		return reconnectDialErrorClosedEOF
	default:
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return reconnectDialErrorTimeout
		}
		return reconnectDialErrorOther
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

func classifyReconnectObservation(snapshot reconnectObservationSnapshot, evidence reconnectCloseEvidence, accepted bool) reconnectClassification {
	if !snapshot.closeMarked || !evidence.markSet || snapshot.overflow > 0 ||
		!evidence.closeCorrelationComplete || !evidence.registryAtResolveRecorded {
		return reconnectInconclusive
	}
	if snapshot.closeMark != evidence.markSeq {
		return reconnectInconclusive
	}

	starts := make(map[uint64]uint64)
	terminals := make(map[uint64]reconnectEvent)
	acceptEvents := 0
	for _, event := range snapshot.events {
		if event.seq <= snapshot.closeMark {
			continue
		}
		switch event.kind {
		case reconnectDialStartEvent:
			starts[event.attempt] = event.seq
		case reconnectDialOKEvent, reconnectDialErrorEvent:
			startSeq, ok := starts[event.attempt]
			if !ok || startSeq >= event.seq {
				return reconnectInconclusive
			}
			if event.kind == reconnectDialOKEvent && (event.localAddr == nil || event.remoteAddr == nil) {
				return reconnectInconclusive
			}
			terminals[event.attempt] = event
		case reconnectAcceptEvent:
			acceptEvents++
			if event.localAddr == nil || event.remoteAddr == nil {
				return reconnectInconclusive
			}
		}
	}
	for ordinal := range starts {
		if _, ok := terminals[ordinal]; !ok {
			return reconnectInconclusive
		}
	}
	if accepted && acceptEvents == 0 {
		return reconnectInconclusive
	}
	for _, event := range snapshot.events {
		if event.seq > snapshot.closeMark && event.kind == reconnectAcceptEvent &&
			acceptMatchesEarlierDial(snapshot, event) {
			return reconnectInconclusive
		}
	}

	matched := matchedReconnectAcceptOrdinals(snapshot)
	if evidence.closedMatchedA1Current == 0 {
		return reconnectCloseMissedA1
	}
	if len(matched) > 0 {
		return reconnectFixtureAcceptMatched
	}
	if len(starts) == 0 {
		return reconnectMissingDial
	}
	latestOrdinal := uint64(0)
	for ordinal := range starts {
		if ordinal > latestOrdinal {
			latestOrdinal = ordinal
		}
	}
	terminal := terminals[latestOrdinal]
	if terminal.kind == reconnectDialErrorEvent {
		return reconnectDialUnsuccessful
	}
	return reconnectDialWithoutFixtureAccept
}

func matchedReconnectAcceptOrdinals(snapshot reconnectObservationSnapshot) map[uint64]struct{} {
	starts := make(map[uint64]uint64)
	for _, event := range snapshot.events {
		if event.seq > snapshot.closeMark && event.kind == reconnectDialStartEvent {
			starts[event.attempt] = event.seq
		}
	}
	matched := make(map[uint64]struct{})
	for _, accept := range snapshot.events {
		if accept.seq <= snapshot.closeMark || accept.kind != reconnectAcceptEvent {
			continue
		}
		for _, dial := range snapshot.events {
			if dial.seq <= snapshot.closeMark || dial.kind != reconnectDialOKEvent || dial.attempt == 0 {
				continue
			}
			if startSeq, ok := starts[dial.attempt]; !ok || startSeq >= dial.seq {
				continue
			}
			if sameReconnectEndpoints(dial.localAddr, dial.remoteAddr, accept.remoteAddr, accept.localAddr) {
				matched[dial.attempt] = struct{}{}
			}
		}
	}
	return matched
}

func formatReconnectSummary(snapshot reconnectObservationSnapshot, evidence reconnectCloseEvidence, waitResolved, accepted bool, classification reconnectClassification) string {
	var summary strings.Builder
	fmt.Fprintf(&summary,
		"A1_RECONNECT_DIAG classification=%s mark_seq=%d wait_resolved=%t accepted=%t close_total=%d close_matched=%d close_already=%d client_open=%d registry_close=%d registry_resolve=%d overflow=%d events=[",
		classification,
		evidence.markSeq,
		waitResolved,
		accepted,
		evidence.closeTotal,
		evidence.closedMatchedA1Current,
		evidence.closedAlready,
		evidence.clientOpen,
		evidence.registryAtClose,
		evidence.registryAtResolve,
		snapshot.overflow,
	)
	first := true
	for _, event := range snapshot.events {
		if !snapshot.closeMarked || event.seq <= snapshot.closeMark {
			continue
		}
		if !first {
			summary.WriteByte(',')
		}
		first = false
		relativeMS := event.at.Sub(snapshot.closeAt).Milliseconds()
		switch event.kind {
		case reconnectStateEvent:
			fmt.Fprintf(&summary, "state_%s#%d@%+dms", event.state.String(), event.seq, relativeMS)
		case reconnectDialStartEvent:
			fmt.Fprintf(&summary, "dial_start#%d@%+dms/attempt=%d", event.seq, relativeMS, event.attempt)
		case reconnectDialOKEvent:
			fmt.Fprintf(&summary, "dial_ok#%d@%+dms/attempt=%d", event.seq, relativeMS, event.attempt)
		case reconnectDialErrorEvent:
			fmt.Fprintf(&summary, "dial_err#%d@%+dms/attempt=%d/class=%s", event.seq, relativeMS, event.attempt, event.errorClass)
		case reconnectAcceptEvent:
			if ordinal := matchedReconnectAcceptOrdinal(snapshot, event); ordinal > 0 {
				fmt.Fprintf(&summary, "accept#%d@%+dms/match=%d", event.seq, relativeMS, ordinal)
			} else {
				fmt.Fprintf(&summary, "accept#%d@%+dms/match=none", event.seq, relativeMS)
			}
		}
	}
	summary.WriteByte(']')
	return summary.String()
}

func matchedReconnectAcceptOrdinal(snapshot reconnectObservationSnapshot, accept reconnectEvent) uint64 {
	for _, dial := range snapshot.events {
		if dial.seq <= snapshot.closeMark || dial.kind != reconnectDialOKEvent || dial.attempt == 0 {
			continue
		}
		if reconnectDialStartedAfterMark(snapshot, dial.attempt, dial.seq) &&
			sameReconnectEndpoints(dial.localAddr, dial.remoteAddr, accept.remoteAddr, accept.localAddr) {
			return dial.attempt
		}
	}
	return 0
}

func acceptMatchesEarlierDial(snapshot reconnectObservationSnapshot, accept reconnectEvent) bool {
	for _, dial := range snapshot.events {
		if dial.kind != reconnectDialOKEvent ||
			!sameReconnectEndpoints(dial.localAddr, dial.remoteAddr, accept.remoteAddr, accept.localAddr) {
			continue
		}
		if dial.seq <= snapshot.closeMark || !reconnectDialStartedAfterMark(snapshot, dial.attempt, dial.seq) {
			return true
		}
	}
	return false
}

func reconnectDialStartedAfterMark(snapshot reconnectObservationSnapshot, ordinal, beforeSeq uint64) bool {
	for _, event := range snapshot.events {
		if event.seq > snapshot.closeMark && event.seq < beforeSeq &&
			event.kind == reconnectDialStartEvent && event.attempt == ordinal {
			return true
		}
	}
	return false
}

func newReconnectObserverSmokeClient(t *testing.T, f *smokeFixture, label, phone string, observer *reconnectObserver) *smokeClient {
	t.Helper()
	sess := &session.StorageMemory{}
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	client := &smokeClient{
		client:  newA1ReconnectObserverClient(f, sess, seen, push, manager, observer),
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
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), "cause="+reconnectContextFailureClass(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), "cause="+reconnectDialErrorClass(client.lifecycle.result.error()).String()))
	}
	managerStarted := time.Now()
	select {
	case <-ready:
	case <-f.ctx.Done():
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), "cause="+reconnectContextFailureClass(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), "cause="+reconnectDialErrorClass(client.lifecycle.result.error()).String()))
	}
	return client
}

func reconnectContextFailureClass(ctx context.Context) string {
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return reconnectDialErrorClass(cause).String()
}

func newA1ReconnectObserverClient(f *smokeFixture, sess *session.StorageMemory, seen, push *updateCollector, manager *updates.Manager, observer *reconnectObserver) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{Dial: observer.dial}),
		SessionStorage: sess,
		UpdateHandler:  observedManagerHandler{observer: push, manager: manager},
		Middlewares: []telegram.Middleware{
			hook.UpdateHook(manager.Handle),
			hook.AffectedHook(manager),
		},
		OnConnectionState: observer.connectionState,
	})
}

func (l *acceptCountingListener) setReconnectObserver(observer *reconnectObserver) {
	l.mu.Lock()
	l.reconnectObserver = observer
	l.mu.Unlock()
}

type acceptCountingListener struct {
	net.Listener

	mu                sync.Mutex
	accepted          uint64
	active            map[*acceptCountingConn]struct{}
	gate              *acceptGate
	reconnectObserver *reconnectObserver
}

type acceptCountingConn struct {
	net.Conn

	once             sync.Once
	closeErr         error
	owner            *acceptCountingListener
	acceptLocalAddr  net.Addr
	acceptRemoteAddr net.Addr
}

type acceptGate struct {
	done        chan struct{}
	arrived     chan struct{}
	arrivedOnce sync.Once
	releaseOnce sync.Once
}

func newAcceptCountingListener(ln net.Listener) *acceptCountingListener {
	return &acceptCountingListener{
		Listener: ln,
		active:   make(map[*acceptCountingConn]struct{}),
	}
}

func (l *acceptCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &acceptCountingConn{Conn: conn, owner: l}
	l.mu.Lock()
	tracked.acceptLocalAddr = conn.LocalAddr()
	tracked.acceptRemoteAddr = conn.RemoteAddr()
	l.accepted++
	l.active[tracked] = struct{}{}
	gate := l.gate
	observer := l.reconnectObserver
	l.mu.Unlock()
	if observer != nil {
		observer.accept(tracked.acceptLocalAddr, tracked.acceptRemoteAddr)
	}
	if gate != nil {
		gate.arrivedOnce.Do(func() { close(gate.arrived) })
		<-gate.done
	}
	return tracked, nil
}

func (l *acceptCountingListener) Close() error {
	l.mu.Lock()
	gate := l.gate
	l.gate = nil
	l.mu.Unlock()
	if gate != nil {
		gate.release()
	}
	return l.Listener.Close()
}

func (l *acceptCountingListener) acceptCount() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepted
}

func (l *acceptCountingListener) activeConnections() []net.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	connections := make([]net.Conn, 0, len(l.active))
	for conn := range l.active {
		connections = append(connections, conn)
	}
	return connections
}

func (l *acceptCountingListener) closeConnections(connections []net.Conn) error {
	for _, conn := range connections {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

func (l *acceptCountingListener) pauseAccept() (func(), <-chan struct{}) {
	gate := &acceptGate{done: make(chan struct{}), arrived: make(chan struct{})}
	l.mu.Lock()
	l.gate = gate
	l.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			l.mu.Lock()
			if l.gate == gate {
				l.gate = nil
			}
			l.mu.Unlock()
			gate.release()
		})
	}
	return release, gate.arrived
}

func (g *acceptGate) release() {
	g.releaseOnce.Do(func() { close(g.done) })
}

func (c *acceptCountingConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.active, c)
		c.owner.mu.Unlock()
	})
	return c.closeErr
}

func fixtureConfigMatchesListener(cfg *tg.Config, dcID int, listener net.Listener) bool {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || cfg == nil || cfg.ThisDC != dcID || len(cfg.DCOptions) != 1 {
		return false
	}
	option := cfg.DCOptions[0]
	return option.ID == dcID && option.IPAddress == addr.IP.String() && option.Port == addr.Port
}

func reconnectAuthKeyIDs(ctx context.Context, st *store.Store, userID int64) (map[int64]struct{}, error) {
	keys, err := st.AuthKeysByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]struct{}, len(keys))
	for _, key := range keys {
		ids[key.ID] = struct{}{}
	}
	return ids, nil
}

func sameReconnectAuthKeyIDs(left, right map[int64]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if _, ok := right[id]; !ok {
			return false
		}
	}
	return true
}

func reconnectHistoryMessageID(t *testing.T, f *smokeFixture, client *smokeClient, peerID int64, text string) int {
	t.Helper()
	var id int
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peerUser(client.id, peerID), Limit: 20,
		})
		if err != nil {
			return err
		}
		history, ok := result.(*tg.MessagesMessages)
		if !ok {
			return errors.New("unexpected history response")
		}
		for _, class := range history.Messages {
			if message, ok := class.(*tg.Message); ok && message.Message == text {
				if id != 0 {
					return errors.New("duplicate history message")
				}
				id = message.ID
			}
		}
		if id == 0 {
			return errors.New("history message missing")
		}
		return nil
	}); err != nil {
		t.Fatalf("%s history check failed (cause=%s)", client.label, reconnectDialErrorClass(err))
	}
	return id
}

func reconnectStatePts(t *testing.T, f *smokeFixture, userID int64) int {
	t.Helper()
	state, err := f.store.State(f.ctx, userID)
	if err != nil {
		t.Fatal("assertion=update_state_lookup")
	}
	return state.Pts
}

func reconnectDifferenceHasMessage(diff *tg.UpdatesDifference, text string, wantID int, peerID int64) bool {
	count := 0
	for _, class := range diff.NewMessages {
		message, ok := class.(*tg.Message)
		if !ok || message.Message != text {
			continue
		}
		peer, ok := message.PeerID.(*tg.PeerUser)
		if !ok || peer.UserID != peerID || message.ID != wantID || !message.Out {
			return false
		}
		count++
	}
	return count == 1
}

func assertReconnectMessage(t *testing.T, ctx context.Context, collector *updateCollector, text string, wantID int, wantOut bool, peerID int64, wantPts int, label string) {
	t.Helper()
	message := recvOrCtx(t, ctx, collector.newMsg, label+" message")
	pts := recvOrCtx(t, ctx, collector.points, label+" pts")
	peer, ok := message.PeerID.(*tg.PeerUser)
	if message.Message != text || message.ID != wantID || message.Out != wantOut || !ok || peer.UserID != peerID {
		t.Fatalf("%s message metadata mismatch (id=%d out=%t)", label, message.ID, message.Out)
	}
	if pts != wantPts {
		t.Fatalf("%s pts = %d, want %d", label, pts, wantPts)
	}
}

func assertReconnectNoMessage(t *testing.T, ctx context.Context, collector *updateCollector, label string) {
	t.Helper()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-collector.newMsg:
		t.Fatalf("%s received an unexpected message", label)
	case <-timer.C:
	case <-ctx.Done():
		t.Fatalf("waiting for %s isolation check: cause=%s", label, reconnectContextFailureClass(ctx))
	}
}

func assertReconnectOriginSuppressed(t *testing.T, ctx context.Context, collector *updateCollector, originText string, originID int, allowedText string, allowedID, allowedPts int, allowedPeer int64, label string) {
	t.Helper()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case message := <-collector.newMsg:
			pts := recvOrCtx(t, ctx, collector.points, label+" push pts")
			if message.Message == originText && message.ID == originID {
				t.Fatalf("%s received its own origin echo (id=%d)", label, message.ID)
			}
			peer, ok := message.PeerID.(*tg.PeerUser)
			if message.Message != allowedText || message.ID != allowedID || !message.Out || !ok || peer.UserID != allowedPeer || pts != allowedPts {
				t.Fatalf("%s received unexpected push metadata (id=%d pts=%d)", label, message.ID, pts)
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting for %s origin-echo check: cause=%s", label, reconnectContextFailureClass(ctx))
		}
	}
}

func statePts(state *tg.UpdatesState) int {
	if state == nil {
		return -1
	}
	return state.Pts
}

func safeReconnectCallFailure(client *smokeClient, err error) string {
	if client.lifecycle.result.finished() {
		return "cause=" + reconnectDialErrorClass(client.lifecycle.result.error()).String()
	}
	return "cause=" + reconnectDialErrorClass(err).String()
}
