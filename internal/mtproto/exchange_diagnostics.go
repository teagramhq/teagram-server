package mtproto

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/transport"
)

type exchangeStage string

const (
	exchangeStageReadReqPQ      exchangeStage = "read_req_pq"
	exchangeStageWriteResPQ     exchangeStage = "write_res_pq"
	exchangeStageReadReqDH      exchangeStage = "read_req_dh"
	exchangeStageDecodeInner    exchangeStage = "decode_inner"
	exchangeStageWriteServerDH  exchangeStage = "write_server_dh"
	exchangeStageReadClientDH   exchangeStage = "read_client_dh"
	exchangeStageDecodeClientDH exchangeStage = "decode_client_dh"
	exchangeStageWriteDHGen     exchangeStage = "write_dh_gen"
)

type exchangeFailureCause string

const (
	exchangeCauseTimeout    exchangeFailureCause = "timeout"
	exchangeCausePeerClose  exchangeFailureCause = "peer_close"
	exchangeCauseUnexpected exchangeFailureCause = "unexpected_type"
	exchangeCauseDecode     exchangeFailureCause = "decode"
	exchangeCauseWrite      exchangeFailureCause = "write"
	exchangeCauseOther      exchangeFailureCause = "other"
)

type exchangeOperation string

const (
	exchangeOperationRead  exchangeOperation = "read"
	exchangeOperationWrite exchangeOperation = "write"
)

type exchangeFailureError struct {
	err            error
	stage          exchangeStage
	cause          exchangeFailureCause
	stageStartedAt time.Time
	gaveUpAt       time.Time
}

func (e *exchangeFailureError) Error() string { return e.err.Error() }
func (e *exchangeFailureError) Unwrap() error { return e.err }

type exchangeDiagnostics struct {
	stage          exchangeStage
	operation      exchangeOperation
	stageStartedAt time.Time
}

type transportCloseStep string

const (
	transportCloseStepNone          transportCloseStep = "none"
	transportCloseStepReqPQReceived transportCloseStep = "req_pq received"
	transportCloseStepResPQSent     transportCloseStep = "resPQ sent"
	transportCloseStepReqDHParams   transportCloseStep = "req_DH_params received"
	transportCloseStepDHComplete    transportCloseStep = "DH complete"
	transportCloseStepAuthorized    transportCloseStep = "authorized"
)

type transportCloseDiagnostics struct {
	transport              string
	step                   transportCloseStep
	connectionID           string
	closedAt               time.Time
	peerAddr               netip.Addr
	authKeyID              [8]byte
	hasAuthKeyID           bool
	sessionID              int64
	hasSessionID           bool
	lastFrameReceivedAt    time.Time
	readDeadline           time.Time
	deadlineSource         string
	exchangeStage          exchangeStage
	exchangeStageStartedAt time.Time
	exchangeGaveUpAt       time.Time
}

type transportCloseReason string

const (
	transportCloseReasonClientClose transportCloseReason = "client close"
	transportCloseReasonServerClose transportCloseReason = "server close"
	transportCloseReasonServerError transportCloseReason = "server error"
	transportCloseReasonTimeout     transportCloseReason = "timeout"
)

func classifyTransportCloseReason(err error) transportCloseReason {
	if err == nil {
		return transportCloseReasonServerClose
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return transportCloseReasonTimeout
	}
	if errors.As(err, &netErr) && netErr.Timeout() {
		return transportCloseReasonTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || isDisconnect(err) {
		return transportCloseReasonClientClose
	}
	return transportCloseReasonServerError
}

func newTransportCloseDiagnostics(transport, connectionID string) *transportCloseDiagnostics {
	return &transportCloseDiagnostics{transport: transport, step: transportCloseStepNone, connectionID: connectionID}
}

func (d *transportCloseDiagnostics) observeRequest(id uint32) {
	switch id {
	case mt.ReqPqMultiRequestTypeID:
		d.step = transportCloseStepReqPQReceived
	case mt.ReqDHParamsRequestTypeID:
		d.step = transportCloseStepReqDHParams
	}
}

func (d *transportCloseDiagnostics) observeResponse(id uint32) {
	if id == mt.ResPQTypeID {
		d.step = transportCloseStepResPQSent
	}
}

func (d *transportCloseDiagnostics) markDHComplete() {
	d.step = transportCloseStepDHComplete
	d.clearExchangeStage()
}

func (d *transportCloseDiagnostics) markAuthorized() {
	d.step = transportCloseStepAuthorized
}

func (d *transportCloseDiagnostics) setPeerAddr(addr netip.Addr) {
	d.peerAddr = addr
}

func (d *transportCloseDiagnostics) observeAuthKeyID(id [8]byte) {
	d.authKeyID = id
	d.hasAuthKeyID = id != ([8]byte{})
}

func (d *transportCloseDiagnostics) observeAuthenticatedFrame(authKeyID [8]byte, sessionID int64) {
	d.observeAuthKeyID(authKeyID)
	d.sessionID = sessionID
	d.hasSessionID = true
}

func (d *transportCloseDiagnostics) observeReadTimeout(readDeadline time.Time, deadlineSource string, closedAt time.Time) {
	d.readDeadline = readDeadline
	d.deadlineSource = deadlineSource
	d.closedAt = closedAt
	if d.exchangeStage != "" {
		d.exchangeGaveUpAt = closedAt
	}
}

func (d *transportCloseDiagnostics) observeExchangeStage(stage exchangeStage, startedAt time.Time) {
	if d.exchangeStage != stage || d.exchangeStageStartedAt.IsZero() {
		d.exchangeStage = stage
		d.exchangeStageStartedAt = startedAt
		d.exchangeGaveUpAt = time.Time{}
	}
}

func (d *transportCloseDiagnostics) clearExchangeStage() {
	d.exchangeStage = ""
	d.exchangeStageStartedAt = time.Time{}
	d.exchangeGaveUpAt = time.Time{}
}

func (d *transportCloseDiagnostics) observeExchangeFailure(stage exchangeStage, startedAt, gaveUpAt time.Time) {
	d.observeExchangeStage(stage, startedAt)
	d.exchangeGaveUpAt = gaveUpAt
	d.closedAt = gaveUpAt
}

func newExchangeDiagnostics() *exchangeDiagnostics {
	return &exchangeDiagnostics{stage: exchangeStageReadReqPQ, stageStartedAt: time.Now()}
}

// exchangeDiagnosticConn observes only fixed protocol constructor IDs from
// existing unencrypted envelopes. Its inspection aliases the buffer and reads
// a fixed-size prefix, so it neither consumes nor copies a frame.
type exchangeDiagnosticConn struct {
	transport.Conn

	diagnostics *exchangeDiagnostics
	closeState  *transportCloseDiagnostics
}

func (c exchangeDiagnosticConn) Recv(ctx context.Context, b *bin.Buffer) error {
	c.diagnostics.operation = exchangeOperationRead
	if c.diagnostics.stageStartedAt.IsZero() {
		c.diagnostics.stageStartedAt = time.Now()
	}
	c.syncStage()
	if err := c.Conn.Recv(ctx, b); err != nil {
		return err
	}
	id, ok := unencryptedConstructor(b)
	if !ok {
		c.syncStage()
		return nil
	}
	if c.closeState != nil {
		c.closeState.observeRequest(id)
	}
	switch id {
	case mt.ReqDHParamsRequestTypeID:
		if c.diagnostics.stage == exchangeStageReadReqDH {
			c.diagnostics.stage = exchangeStageDecodeInner
			c.diagnostics.stageStartedAt = time.Now()
		}
	case mt.SetClientDHParamsRequestTypeID:
		if c.diagnostics.stage == exchangeStageReadClientDH {
			c.diagnostics.stage = exchangeStageDecodeClientDH
			c.diagnostics.stageStartedAt = time.Now()
		}
	}
	c.syncStage()
	return nil
}

func (c exchangeDiagnosticConn) Send(ctx context.Context, b *bin.Buffer) error {
	c.diagnostics.operation = exchangeOperationWrite
	startedAt := time.Now()
	id, ok := unencryptedConstructor(b)
	if !ok {
		return c.Conn.Send(ctx, b)
	}
	var next exchangeStage
	switch id {
	case mt.ResPQTypeID:
		c.diagnostics.stage = exchangeStageWriteResPQ
		c.diagnostics.stageStartedAt = startedAt
		next = exchangeStageReadReqDH
	case mt.ServerDHParamsOkTypeID:
		c.diagnostics.stage = exchangeStageWriteServerDH
		c.diagnostics.stageStartedAt = startedAt
		next = exchangeStageReadClientDH
	case mt.DhGenOkTypeID:
		c.diagnostics.stage = exchangeStageWriteDHGen
		c.diagnostics.stageStartedAt = startedAt
	}
	c.syncStage()
	err := c.Conn.Send(ctx, b)
	if err == nil && next != "" {
		c.diagnostics.stage = next
		c.diagnostics.stageStartedAt = time.Now()
	}
	if err == nil && c.closeState != nil {
		c.closeState.observeResponse(id)
	}
	c.syncStage()
	return err
}

func (c exchangeDiagnosticConn) syncStage() {
	if c.closeState != nil {
		c.closeState.observeExchangeStage(c.diagnostics.stage, c.diagnostics.stageStartedAt)
	}
}

func unencryptedConstructor(b *bin.Buffer) (uint32, bool) {
	if b == nil || b.Len() < 20 {
		return 0, false
	}
	frame := bin.Buffer{Buf: b.Buf}
	var authKeyID [8]byte
	if err := frame.PeekN(authKeyID[:], len(authKeyID)); err != nil || authKeyID != [8]byte{} {
		return 0, false
	}
	frame.Skip(16) // auth_key_id and message_id
	dataLen, err := frame.Int32()
	if err != nil || dataLen < 4 || int64(dataLen) > int64(frame.Len()) {
		return 0, false
	}
	id, err := frame.PeekID()
	return id, err == nil
}

func classifyExchangeFailure(err error, operation exchangeOperation) exchangeFailureCause {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return exchangeCauseTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.ErrClosedPipe), errors.Is(err, net.ErrClosed):
		return exchangeCausePeerClose
	}
	if unexpected, ok := errors.AsType[*bin.UnexpectedIDErr](err); ok && unexpected != nil {
		return exchangeCauseUnexpected
	}
	if invalidLength, ok := errors.AsType[*bin.InvalidLengthError](err); ok && invalidLength != nil {
		return exchangeCauseDecode
	}
	if operation == exchangeOperationWrite {
		return exchangeCauseWrite
	}
	return exchangeCauseOther
}
