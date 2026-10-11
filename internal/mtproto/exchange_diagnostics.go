package mtproto

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

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
	err   error
	stage exchangeStage
	cause exchangeFailureCause
}

func (e *exchangeFailureError) Error() string { return e.err.Error() }
func (e *exchangeFailureError) Unwrap() error { return e.err }

type exchangeDiagnostics struct {
	stage     exchangeStage
	operation exchangeOperation
}

func newExchangeDiagnostics() *exchangeDiagnostics {
	return &exchangeDiagnostics{stage: exchangeStageReadReqPQ}
}

// exchangeDiagnosticConn observes only fixed protocol constructor IDs from
// existing unencrypted envelopes. Its inspection aliases the buffer and reads
// a fixed-size prefix, so it neither consumes nor copies a frame.
type exchangeDiagnosticConn struct {
	transport.Conn

	diagnostics *exchangeDiagnostics
}

func (c exchangeDiagnosticConn) Recv(ctx context.Context, b *bin.Buffer) error {
	c.diagnostics.operation = exchangeOperationRead
	if err := c.Conn.Recv(ctx, b); err != nil {
		return err
	}
	id, ok := unencryptedConstructor(b)
	if !ok {
		return nil
	}
	switch id {
	case mt.ReqDHParamsRequestTypeID:
		if c.diagnostics.stage == exchangeStageReadReqDH {
			c.diagnostics.stage = exchangeStageDecodeInner
		}
	case mt.SetClientDHParamsRequestTypeID:
		if c.diagnostics.stage == exchangeStageReadClientDH {
			c.diagnostics.stage = exchangeStageDecodeClientDH
		}
	}
	return nil
}

func (c exchangeDiagnosticConn) Send(ctx context.Context, b *bin.Buffer) error {
	c.diagnostics.operation = exchangeOperationWrite
	id, ok := unencryptedConstructor(b)
	if !ok {
		return c.Conn.Send(ctx, b)
	}
	var next exchangeStage
	switch id {
	case mt.ResPQTypeID:
		c.diagnostics.stage = exchangeStageWriteResPQ
		next = exchangeStageReadReqDH
	case mt.ServerDHParamsOkTypeID:
		c.diagnostics.stage = exchangeStageWriteServerDH
		next = exchangeStageReadClientDH
	case mt.DhGenOkTypeID:
		c.diagnostics.stage = exchangeStageWriteDHGen
	}
	err := c.Conn.Send(ctx, b)
	if err == nil && next != "" {
		c.diagnostics.stage = next
	}
	return err
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
