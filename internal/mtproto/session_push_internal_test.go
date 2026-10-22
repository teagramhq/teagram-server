package mtproto

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/transport"
)

type sessionPushTestTransport struct {
	transport.Conn

	sends  atomic.Int64
	onSend func()
}

func (c *sessionPushTestTransport) Send(context.Context, *bin.Buffer) error {
	c.sends.Add(1)
	if c.onSend != nil {
		c.onSend()
	}
	return nil
}

func TestPushToOtherSessionsExcludesCallerAndChecksOwnerAfterSnapshot(t *testing.T) {
	key := crypto.Key{1, 2, 3, 4}.WithID()
	newConn := func(tconn *sessionPushTestTransport) *Conn {
		conn := newConn(tconn, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
		conn.setKey(key)
		conn.setSession(1)
		conn.setOwner(7)
		return conn
	}

	registry := NewSessionRegistry()
	callerTransport := &sessionPushTestTransport{}
	firstTransport := &sessionPushTestTransport{}
	reboundTransport := &sessionPushTestTransport{}
	caller := newConn(callerTransport)
	first := newConn(firstTransport)
	rebound := newConn(reboundTransport)
	caller.sessionRegistry = registry
	for _, conn := range []*Conn{caller, first, rebound} {
		if !registry.Add(7, conn) {
			t.Fatal("register owner connection")
		}
	}
	firstTransport.onSend = func() { rebound.setOwner(8) }

	if err := caller.PushToOtherSessions(context.Background(), 7, &mt.Pong{PingID: 19}); err != nil {
		t.Fatalf("push to other sessions: %v", err)
	}
	if got := callerTransport.sends.Load(); got != 0 {
		t.Fatalf("caller session writes = %d, want excluded", got)
	}
	if got := firstTransport.sends.Load(); got != 1 {
		t.Fatalf("same-owner session writes = %d, want one", got)
	}
	if got := reboundTransport.sends.Load(); got != 0 {
		t.Fatalf("rebound session writes = %d, want owner-checked skip", got)
	}
}
