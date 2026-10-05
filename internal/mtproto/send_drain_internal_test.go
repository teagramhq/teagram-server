package mtproto

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
)

type drainWriteTestTransport struct {
	started   chan struct{}
	closed    chan struct{}
	release   chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	sendCalls atomic.Int64
	sendErr   error
}

func newDrainWriteTestTransport() *drainWriteTestTransport {
	return &drainWriteTestTransport{started: make(chan struct{}), closed: make(chan struct{})}
}

func (c *drainWriteTestTransport) Send(ctx context.Context, _ *bin.Buffer) error {
	if c.sendCalls.Add(1) == 1 {
		c.startOnce.Do(func() { close(c.started) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return errors.New("closed")
		case <-c.release:
			return c.sendErr
		}
	}
	return ctx.Err()
}

func (*drainWriteTestTransport) Recv(context.Context, *bin.Buffer) error { return errors.New("unused") }

func (c *drainWriteTestTransport) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestQueuedPushUsesAbsoluteDrainWriteDeadline(t *testing.T) {
	shutdown := newServerShutdown()
	shutdown.drainTimeout = 180 * time.Millisecond
	shutdown.retirementWindow = 60 * time.Millisecond
	shutdown.startServing()
	defer shutdown.finishServing()

	key := crypto.Key{1, 2, 3, 4}.WithID()
	transport := newDrainWriteTestTransport()
	conn := newConn(transport, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
	conn.shutdown = shutdown
	conn.setKey(key)
	conn.setSession(42)
	conn.setOwner(7)

	firstDone := make(chan error, 1)
	go func() { firstDone <- conn.send(context.Background(), proto.MessageFromServer, &mt.Pong{PingID: 1}) }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("first write did not enter the transport")
	}

	shutdown.beginDrain()
	pushDone := make(chan error, 1)
	go func() {
		pushed, err := conn.PushTo(context.Background(), 7, &mt.Pong{PingID: 2}, 0)
		if pushed {
			pushDone <- errors.New("queued push was written after the drain output deadline")
			return
		}
		pushDone <- err
	}()

	select {
	case <-shutdown.outputCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("shared drain output deadline did not expire")
	}
	for _, item := range []struct {
		name string
		done <-chan error
	}{{name: "first write", done: firstDone}, {name: "queued push", done: pushDone}} {
		select {
		case err := <-item.done:
			if err == nil {
				t.Fatalf("%s succeeded after the shared output deadline", item.name)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("%s remained blocked after the shared output deadline", item.name)
		}
	}
	if calls := transport.sendCalls.Load(); calls != 1 {
		t.Fatalf("transport sends = %d, want only the first write", calls)
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("stalled queued output did not close its transport")
	}
}

func TestReplyAndQueuedPushShareDrainWriteDeadline(t *testing.T) {
	for _, releaseReply := range []bool{true, false} {
		name := "stalled reply"
		if releaseReply {
			name = "healthy reply"
		}
		t.Run(name, func(t *testing.T) {
			shutdown := newServerShutdown()
			shutdown.drainTimeout = 250 * time.Millisecond
			shutdown.retirementWindow = 50 * time.Millisecond
			shutdown.startServing()
			defer shutdown.finishServing()

			transport := newDrainWriteTestTransport()
			var releaseReplyWrite func()
			if releaseReply {
				release := make(chan struct{})
				transport.release = release
				var releaseOnce sync.Once
				releaseReplyWrite = func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseReplyWrite)
			}
			key := crypto.Key{1, 2, 3, 4}.WithID()
			conn := newConn(transport, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
			conn.shutdown = shutdown
			conn.setKey(key)
			conn.setSession(42)
			conn.setOwner(7)
			shutdown.beginDrain()

			// Spend most of the shared window before the reply is queued, as an
			// admitted RPC near its execution deadline would.
			select {
			case <-time.After(120 * time.Millisecond):
			case <-shutdown.outputCtx.Done():
				t.Fatal("output deadline expired before the reply was queued")
			}
			req := &Request{Ctx: context.Background(), MsgID: 1 << 32, SessionID: 42}
			replyDone := make(chan error, 1)
			go func() { replyDone <- conn.SendResult(req, &mt.Pong{PingID: 1}) }()
			select {
			case <-transport.started:
			case <-shutdown.outputCtx.Done():
				t.Fatal("reply did not reach the transport before the output deadline")
			}

			pushStarted := make(chan struct{})
			pushDone := make(chan error, 1)
			go func() {
				close(pushStarted)
				pushed, err := conn.PushTo(context.Background(), 7, &mt.Pong{PingID: 2}, 0)
				if pushed {
					pushDone <- nil
					return
				}
				if err == nil {
					pushDone <- errors.New("queued push was not attempted")
					return
				}
				pushDone <- err
			}()
			<-pushStarted

			if releaseReply {
				select {
				case <-time.After(15 * time.Millisecond):
				case <-shutdown.outputCtx.Done():
					t.Fatal("output deadline expired while the reply was released within budget")
				}
				releaseReplyWrite()
			} else {
				select {
				case <-shutdown.outputCtx.Done():
				case <-time.After(time.Second):
					t.Fatal("shared output deadline did not expire")
				}
			}

			for _, result := range []struct {
				name string
				done <-chan error
			}{{name: "reply", done: replyDone}, {name: "queued push", done: pushDone}} {
				select {
				case err := <-result.done:
					if releaseReply && err != nil {
						t.Fatalf("%s failed inside the shared output window: %v", result.name, err)
					}
					if !releaseReply && err == nil {
						t.Fatalf("%s succeeded after the shared output deadline", result.name)
					}
				case <-time.After(100 * time.Millisecond):
					t.Fatalf("%s remained blocked after the output deadline", result.name)
				}
			}

			wantCalls := int64(2)
			if !releaseReply {
				wantCalls = 1
				select {
				case <-transport.closed:
				default:
					t.Fatal("stalled queued output did not close its transport")
				}
			}
			if calls := transport.sendCalls.Load(); calls != wantCalls {
				t.Fatalf("transport sends = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestServerShutdownSkipsRetirementAfterQueuedPushClosesTransport(t *testing.T) {
	shutdown := newServerShutdown()
	shutdown.drainTimeout = 10 * time.Second
	shutdown.retirementWindow = time.Second
	shutdown.retirementSlots = 2
	shutdown.startServing()
	defer shutdown.finishServing()
	defer stopShutdownTimers(shutdown)
	shutdown.beginDrain()

	transport := newDrainWriteTestTransport()
	transport.release = make(chan struct{})
	writeErr := errors.New("injected push write failure")
	transport.sendErr = writeErr
	key := crypto.Key{1, 2, 3, 4}.WithID()
	conn := newConn(transport, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
	conn.shutdown = shutdown
	conn.setKey(key)
	conn.setSession(42)
	conn.setOwner(7)

	pushDone := make(chan error, 1)
	go func() {
		_, err := conn.PushTo(context.Background(), 7, &mt.Pong{PingID: 99}, 0)
		pushDone <- err
	}()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("queued push did not enter the transport")
	}

	pushesDone := conn.stopPushAdmission()
	close(transport.release)
	select {
	case err := <-pushDone:
		if !errors.Is(err, writeErr) {
			t.Fatalf("failed push error = %v, want injected write failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed queued push did not return")
	}
	select {
	case <-pushesDone:
	case <-time.After(time.Second):
		t.Fatal("failed queued push did not finish drain accounting")
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("failed queued push did not close the transport")
	}
	if !conn.transportClosed.Load() {
		t.Fatal("failed queued push did not record the transport close")
	}

	// This is the pushes-done-first branch in serveConn's retirement path.
	shutdown.retirementSeq.Store(1)
	started := time.Now()
	shutdown.waitForRetirement(conn.transportClosed.Load())
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("retirement waited %s after the pushes-done-first close", elapsed)
	}
}
