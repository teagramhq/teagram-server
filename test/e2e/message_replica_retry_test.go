package e2e_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestMessagingRetryAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store close: %v", cerr)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	writeGate := &serverWriteGate{}
	base1 := mustListen(t, ctx, "127.0.0.1:0")
	ln1 := &serverWriteGateListener{Listener: base1, gate: writeGate}
	ln2 := mustListen(t, ctx, "127.0.0.1:0")
	port1, port2 := tcpPort(t, ln1), tcpPort(t, ln2)
	t.Cleanup(bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), ln1))
	t.Cleanup(bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), ln2))

	const phoneA, phoneB, phoneC, phoneD = "+15551265001", "+15551265002", "+15551265003", "+15551265004"
	seedUsernameUsers(t, ctx, st, phoneA, phoneB, phoneC, phoneD)
	aID := replicaUserID(t, ctx, st, phoneA)
	bID := replicaUserID(t, ctx, st, phoneB)
	cID := replicaUserID(t, ctx, st, phoneC)
	dID := replicaUserID(t, ctx, st, phoneD)

	aSession := &session.StorageMemory{}
	aClient := newReplicaClient(key, dcID, port1, aSession, nil, nil)
	aCtx, cancelA := context.WithCancel(ctx)
	t.Cleanup(cancelA)
	readyToSend := make(chan int64, 1)
	sendNow := make(chan struct{})
	aSendErr := make(chan error, 1)
	aRunErr := make(chan error, 1)
	const text = "committed before reply loss"
	const randomID int64 = 1265001
	go func() {
		aRunErr <- aClient.Run(aCtx, func(runCtx context.Context) error {
			if err := aClient.Auth().IfNecessary(runCtx, replicaAuthFlow(codes, phoneA)); err != nil {
				return err
			}
			self, err := aClient.Self(runCtx)
			if err != nil {
				return err
			}
			readyToSend <- self.ID
			select {
			case <-sendNow:
			case <-runCtx.Done():
				return runCtx.Err()
			}
			_, err = aClient.API().MessagesSendMessage(runCtx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(self.ID, bID), Message: text, RandomID: randomID,
			})
			aSendErr <- err
			return err
		})
	}()
	if got := recvOrCtx(t, ctx, readyToSend, "A login on replica 1"); got != aID {
		t.Fatalf("A identity = %d, want %d", got, aID)
	}
	keysBefore, err := st.AuthKeysByUser(ctx, aID)
	if err != nil || len(keysBefore) != 1 {
		t.Fatalf("A auth keys before send = %d, err=%v; want one", len(keysBefore), err)
	}
	attempt, err := writeGate.arm()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { attempt.decide(true) })
	close(sendNow)
	recvOrCtx(t, ctx, attempt.entered, "A's committed send response write")

	originalA := assertSingleOwnerMessage(t, ctx, st, aID, bID, text, true, 1)
	assertSingleOwnerMessage(t, ctx, st, bID, aID, text, false, 1)
	// Stop gotd from transparently retrying this failed call back to replica 1.
	cancelA()
	attempt.decide(true)
	if err := recvOrCtx(t, ctx, aSendErr, "A send with its response dropped"); err == nil {
		t.Fatal("A send RPC succeeded even though its response was dropped")
	}
	if err := recvOrCtx(t, ctx, aRunErr, "A run after its response was dropped"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("A run after dropping the send response: %v", err)
	}

	// Reusing the same gotd session storage sends the original auth key to replica 2.
	retryClient := newReplicaClient(key, dcID, port2, aSession, nil, nil)
	var retryResult tg.UpdatesClass
	var retryUserID int64
	if err := retryClient.Run(ctx, func(runCtx context.Context) error {
		status, err := retryClient.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		if !status.Authorized || status.User == nil || status.User.ID != aID {
			return fmt.Errorf("reconnected auth status = %+v, want authorized user %d", status, aID)
		}
		self, err := retryClient.Self(runCtx)
		if err != nil {
			return err
		}
		retryUserID = self.ID
		if self.ID != aID {
			return fmt.Errorf("reconnected identity = %d, want %d", self.ID, aID)
		}
		retryResult, err = retryClient.API().MessagesSendMessage(runCtx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(aID, bID), Message: text, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("A retry on replica 2: %v", err)
	}
	if retryUserID != aID {
		t.Fatalf("retry user id = %d, want %d", retryUserID, aID)
	}
	assertRetryResult(t, retryResult, text, bID, randomID, int(originalA.LocalID), 1)
	assertSingleOwnerMessage(t, ctx, st, aID, bID, text, true, 1)
	assertSingleOwnerMessage(t, ctx, st, bID, aID, text, false, 1)
	assertSameAuthKey(t, ctx, st, aID, keysBefore)

	// A second owner may use the same random_id without seeing or reusing A's row.
	cClient := newReplicaClient(key, dcID, port2, &session.StorageMemory{}, nil, nil)
	const otherText = "same random id owned by C"
	var otherResult tg.UpdatesClass
	if err := cClient.Run(ctx, func(runCtx context.Context) error {
		if err := cClient.Auth().IfNecessary(runCtx, replicaAuthFlow(codes, phoneC)); err != nil {
			return err
		}
		self, err := cClient.Self(runCtx)
		if err != nil {
			return err
		}
		if self.ID != cID {
			return fmt.Errorf("C identity = %d, want %d", self.ID, cID)
		}
		otherResult, err = cClient.API().MessagesSendMessage(runCtx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(cID, dID), Message: otherText, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("C send with A's random_id: %v", err)
	}
	assertRetryResult(t, otherResult, otherText, dID, randomID, 1, 1)
	assertOwnerScopedSendResult(t, otherResult, cID, dID, otherText, randomID, 1, 1)
	assertSingleOwnerMessage(t, ctx, st, cID, dID, otherText, true, 1)
	assertSingleOwnerMessage(t, ctx, st, dID, cID, otherText, false, 1)
	if got, ok, err := st.MessageByRandomID(ctx, aID, randomID); err != nil || !ok || got.Text != text {
		t.Fatalf("A random_id row = %+v, found=%v err=%v", got, ok, err)
	}
	if got, ok, err := st.MessageByRandomID(ctx, cID, randomID); err != nil || !ok || got.Text != otherText {
		t.Fatalf("C random_id row = %+v, found=%v err=%v", got, ok, err)
	}
}

func TestMessagingCatchUpAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store close: %v", cerr)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	trackedA := newAcceptCountingListener(mustListen(t, ctx, "127.0.0.1:0"))
	lnB := mustListen(t, ctx, "127.0.0.1:0")
	portA, portB := tcpPort(t, trackedA), tcpPort(t, lnB)
	registryA, stopA := bootServerWithRegistry(t, ctx, key, dcID, st, dsn, codes.Logger(), trackedA)
	t.Cleanup(stopA)
	t.Cleanup(bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), lnB))

	const phoneA, phoneB = "+15551265101", "+15551265102"
	seedUsernameUsers(t, ctx, st, phoneA, phoneB)
	aID := replicaUserID(t, ctx, st, phoneA)
	bID := replicaUserID(t, ctx, st, phoneB)
	aSession := &session.StorageMemory{}
	collector := newUpdateCollector()
	manager := updates.New(updates.Config{Handler: collector})
	managerMiddleware := []telegram.Middleware{hook.UpdateHook(manager.Handle), hook.AffectedHook(manager)}
	aClient := newReplicaClient(key, dcID, portA, aSession, manager, managerMiddleware)
	type initialSession struct {
		userID int64
		state  *tg.UpdatesState
	}
	aReady := make(chan initialSession, 1)
	disconnectA := make(chan struct{})
	aRunErr := make(chan error, 1)
	go func() {
		aRunErr <- aClient.Run(ctx, func(runCtx context.Context) error {
			if err := aClient.Auth().IfNecessary(runCtx, replicaAuthFlow(codes, phoneA)); err != nil {
				return err
			}
			self, err := aClient.Self(runCtx)
			if err != nil {
				return err
			}
			state, err := aClient.API().UpdatesGetState(runCtx)
			if err != nil {
				return err
			}
			managerCtx, cancelManager := context.WithCancel(runCtx)
			managerStarted := make(chan struct{})
			managerDone := make(chan error, 1)
			go func() {
				managerDone <- manager.Run(managerCtx, aClient.API(), aID, updates.AuthOptions{
					Forget:  true,
					OnStart: func(context.Context) { close(managerStarted) },
				})
			}()
			select {
			case <-managerStarted:
			case err := <-managerDone:
				cancelManager()
				return fmt.Errorf("start initial update manager: %w", err)
			case <-runCtx.Done():
				cancelManager()
				return runCtx.Err()
			}
			aReady <- initialSession{userID: self.ID, state: state}
			select {
			case <-disconnectA:
			case <-runCtx.Done():
			}
			cancelManager()
			if err := <-managerDone; err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("stop initial update manager: %w", err)
			}
			return nil
		})
	}()
	initial := recvOrCtx(t, ctx, aReady, "A initial session on replica 1")
	if initial.userID != aID || initial.state == nil || initial.state.Pts != 0 {
		t.Fatalf("A initial identity/state = %d/%+v, want %d/pts 0", initial.userID, initial.state, aID)
	}
	waitForOwnerConnections(t, ctx, registryA, aID, 1, "replica 1")
	keysBefore, err := st.AuthKeysByUser(ctx, aID)
	if err != nil || len(keysBefore) != 1 {
		t.Fatalf("A auth keys before interruption = %d, err=%v; want one", len(keysBefore), err)
	}
	if err := trackedA.closeConnections(trackedA.activeConnections()); err != nil {
		t.Fatalf("interrupt A delivery: %v", err)
	}
	close(disconnectA)
	// The socket reader can report EOF before the callback cancels the client.
	if err := recvOrCtx(t, ctx, aRunErr, "A disconnected from replica 1"); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		t.Fatalf("A run after intentional disconnect: %v", err)
	}
	manager.Reset()
	waitNoConn(t, registryA, aID, 3*time.Second, "after A delivery interruption")

	bClient := newReplicaClient(key, dcID, portB, &session.StorageMemory{}, nil, nil)
	const text = "committed while A was disconnected"
	const randomID int64 = 1265101
	var sent tg.UpdatesClass
	if err := bClient.Run(ctx, func(runCtx context.Context) error {
		if err := bClient.Auth().IfNecessary(runCtx, replicaAuthFlow(codes, phoneB)); err != nil {
			return err
		}
		self, err := bClient.Self(runCtx)
		if err != nil {
			return err
		}
		if self.ID != bID {
			return fmt.Errorf("B identity = %d, want %d", self.ID, bID)
		}
		sent, err = bClient.API().MessagesSendMessage(runCtx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(bID, aID), Message: text, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("B send while A was disconnected: %v", err)
	}
	assertRetryResult(t, sent, text, aID, randomID, 1, 1)
	assertSingleOwnerMessage(t, ctx, st, bID, aID, text, true, 1)
	assertSingleOwnerMessage(t, ctx, st, aID, bID, text, false, 1)

	aClientOnB := newReplicaClient(
		key, dcID, portB, aSession, manager,
		managerMiddleware,
	)
	stopA2 := make(chan struct{})
	type managedSession struct {
		userID int64
		api    *tg.Client
	}
	managedReady := make(chan managedSession, 1)
	a2RunErr := make(chan error, 1)
	go func() {
		a2RunErr <- aClientOnB.Run(ctx, func(runCtx context.Context) error {
			status, err := aClientOnB.Auth().Status(runCtx)
			if err != nil {
				return err
			}
			if !status.Authorized || status.User == nil || status.User.ID != aID {
				return fmt.Errorf("reconnected auth status = %+v, want authorized user %d", status, aID)
			}
			self, err := aClientOnB.Self(runCtx)
			if err != nil {
				return err
			}
			if self.ID != aID {
				return fmt.Errorf("reconnected identity = %d, want %d", self.ID, aID)
			}
			managerCtx, cancelManager := context.WithCancel(runCtx)
			defer cancelManager()
			managerStarted := make(chan struct{})
			managerDone := make(chan error, 1)
			go func() {
				managerDone <- manager.Run(managerCtx, aClientOnB.API(), aID, updates.AuthOptions{
					Forget:  false,
					OnStart: func(context.Context) { close(managerStarted) },
				})
			}()
			select {
			case <-managerStarted:
			case err := <-managerDone:
				return fmt.Errorf("start update manager: %w", err)
			case <-runCtx.Done():
				return runCtx.Err()
			}
			managedReady <- managedSession{userID: self.ID, api: aClientOnB.API()}
			select {
			case <-stopA2:
			case <-runCtx.Done():
			}
			cancelManager()
			if err := <-managerDone; err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("stop update manager: %w", err)
			}
			return nil
		})
	}()
	managed := recvOrCtx(t, ctx, managedReady, "A reconnect and update manager on replica 2")
	if managed.userID != aID {
		t.Fatalf("managed A identity = %d, want %d", managed.userID, aID)
	}
	caughtUp := recvOrCtx(t, ctx, collector.newMsg, "A getDifference catch-up")
	peer, ok := caughtUp.PeerID.(*tg.PeerUser)
	if caughtUp.Message != text || caughtUp.Out || !ok || peer.UserID != bID {
		t.Fatalf("A applied catch-up message = %+v, want incoming %q from B", caughtUp, text)
	}
	markerUserID := aID + bID + 99_991
	catchUpMarker := &tg.UpdateUserStatus{UserID: markerUserID, Status: &tg.UserStatusOnline{Expires: int(time.Now().Add(time.Minute).Unix())}}
	if err := manager.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{catchUpMarker}}); err != nil {
		t.Fatalf("client manager catch-up barrier: %v", err)
	}
	if marker := recvOrCtx(t, ctx, collector.userStatus, "client manager catch-up barrier"); marker.UserID != markerUserID {
		t.Fatalf("catch-up barrier user = %d, want %d", marker.UserID, markerUserID)
	}
	assertSingleOwnerMessage(t, ctx, st, bID, aID, text, true, 1)
	assertSingleOwnerMessage(t, ctx, st, aID, bID, text, false, 1)
	assertSameAuthKey(t, ctx, st, aID, keysBefore)

	// The protocol can replay an event for a stale cursor. Feed that same pts to
	// the real gotd manager and assert it does not apply the message twice.
	diffClass, err := managed.api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{
		Pts: initial.state.Pts, Qts: initial.state.Qts, Date: initial.state.Date,
	})
	if err != nil {
		t.Fatalf("stale cursor getDifference: %v", err)
	}
	diff, ok := diffClass.(*tg.UpdatesDifference)
	if !ok || diff == nil {
		t.Fatalf("stale cursor difference = %T, want *tg.UpdatesDifference", diffClass)
	}
	replayedMessage, ok := onlyDifferenceMessage(t, diff, text, bID)
	if !ok {
		t.Fatalf("stale cursor difference did not contain exactly one %q message", text)
	}
	events, err := st.EventsSince(ctx, aID, initial.state.Pts)
	if err != nil || len(events) != 1 || events[0].Pts != 1 || events[0].Type != store.EventNewMessage {
		t.Fatalf("A committed events after its old cursor = %+v, err=%v; want one pts-1 message", events, err)
	}
	duplicate := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateNewMessage{Message: replayedMessage, Pts: events[0].Pts, PtsCount: 1},
			&tg.UpdateUserStatus{UserID: markerUserID, Status: &tg.UserStatusOnline{Expires: int(time.Now().Add(time.Minute).Unix())}},
		},
		Users: diff.Users,
		Date:  diff.State.Date,
	}
	if err := manager.Handle(ctx, duplicate); err != nil {
		t.Fatalf("apply replayed event to client manager: %v", err)
	}
	marker := recvOrCtx(t, ctx, collector.userStatus, "client manager replay barrier")
	if marker.UserID != markerUserID {
		t.Fatalf("replay barrier user = %d, want %d", marker.UserID, markerUserID)
	}
	if got := takeMessage(t, collector.newMsg); got != nil {
		t.Fatalf("client applied the replayed message twice: %+v", got)
	}
	close(stopA2)
	if err := recvOrCtx(t, ctx, a2RunErr, "A manager stopped on replica 2"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("A run after catch-up: %v", err)
	}
}

func newReplicaClient(key *rsa.PrivateKey, dcID, port int, sess *session.StorageMemory, handler telegram.UpdateHandler, middlewares []telegram.Middleware) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: dcID, IPAddress: "127.0.0.1", Port: port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: sess,
		UpdateHandler:  handler,
		Middlewares:    middlewares,
	})
}

func replicaAuthFlow(codes *multiCodeSink, phone string) auth.Flow {
	return auth.NewFlow(
		auth.Constant(smokeUsernameForPhone(phone), smokeUsernamePassword, auth.CodeAuthenticatorFunc(
			func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
				return codes.wait(ctx, phone)
			})),
		auth.SendCodeOptions{},
	)
}

func replicaUserID(t *testing.T, ctx context.Context, st *store.Store, phone string) int64 {
	t.Helper()
	user, ok, err := usernameUserByIdentity(ctx, st, phone)
	if err != nil || !ok {
		t.Fatalf("lookup seeded user %s: found=%v err=%v", phone, ok, err)
	}
	return user.ID
}

func assertSingleOwnerMessage(t *testing.T, ctx context.Context, st *store.Store, ownerID, peerID int64, text string, outgoing bool, wantPts int) store.Message {
	t.Helper()
	state, err := st.State(ctx, ownerID)
	if err != nil {
		t.Fatalf("state for owner %d: %v", ownerID, err)
	}
	if state.Pts != wantPts {
		t.Fatalf("owner %d pts = %d, want %d", ownerID, state.Pts, wantPts)
	}
	history, err := st.History(ctx, ownerID, store.PeerTypeUser, peerID, 0, 10)
	if err != nil {
		t.Fatalf("history for owner %d: %v", ownerID, err)
	}
	if len(history) != 1 {
		t.Fatalf("owner %d history with %d has %d messages, want one", ownerID, peerID, len(history))
	}
	message := history[0]
	if message.Text != text || message.Out != outgoing {
		t.Fatalf("owner %d history message = %+v, want text %q outgoing=%t", ownerID, message, text, outgoing)
	}
	events, err := st.EventsSince(ctx, ownerID, 0)
	if err != nil {
		t.Fatalf("events for owner %d: %v", ownerID, err)
	}
	if len(events) != 1 || events[0].Pts != wantPts || events[0].Type != store.EventNewMessage || events[0].LocalID != message.LocalID {
		t.Fatalf("owner %d events = %+v, want one pts-%d new-message for local id %d", ownerID, events, wantPts, message.LocalID)
	}
	return message
}

func assertRetryResult(t *testing.T, result tg.UpdatesClass, text string, peerID int64, randomID int64, wantID, wantPts int) {
	t.Helper()
	message, pts, ok := outgoingMessage(t, result, text)
	if !ok || countOutgoingMessages(result, text) != 1 {
		t.Fatalf("send result = %T, want exactly one outgoing %q", result, text)
	}
	peer, ok := message.PeerID.(*tg.PeerUser)
	if !ok || peer.UserID != peerID || message.ID != wantID || pts != wantPts {
		t.Fatalf("outgoing result = {id:%d pts:%d peer:%v}, want id=%d pts=%d peer=%d", message.ID, pts, message.PeerID, wantID, wantPts, peerID)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", result)
	}
	messageIDs := 0
	for _, update := range updates.Updates {
		if mapped, ok := update.(*tg.UpdateMessageID); ok && mapped.RandomID == randomID && mapped.ID == wantID {
			messageIDs++
		}
	}
	if messageIDs != 1 {
		t.Fatalf("send result has %d matching random_id mappings, want one", messageIDs)
	}
}

func assertOwnerScopedSendResult(t *testing.T, result tg.UpdatesClass, ownerID, peerID int64, text string, randomID int64, wantID, wantPts int) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", result)
	}
	users := make(map[int64]bool, len(updates.Users))
	for _, user := range updates.Users {
		users[user.GetID()] = true
	}
	if len(updates.Users) != 2 || len(users) != 2 || !users[ownerID] || !users[peerID] {
		t.Fatalf("owner %d result users = %v, want only owner=%d and peer=%d", ownerID, users, ownerID, peerID)
	}
	if len(updates.Chats) != 0 {
		t.Fatalf("owner %d result contains unexpected chats: %+v", ownerID, updates.Chats)
	}

	newMessages, messageIDs := 0, 0
	for i, class := range updates.Updates {
		switch update := class.(type) {
		case *tg.UpdateNewMessage:
			message, ok := update.Message.(*tg.Message)
			if !ok {
				t.Fatalf("owner %d result update %d message = %T, want *tg.Message", ownerID, i, update.Message)
			}
			peer, ok := message.PeerID.(*tg.PeerUser)
			if !ok || peer.UserID != peerID || message.ID != wantID || message.Message != text || !message.Out || update.Pts != wantPts || update.PtsCount != 1 {
				t.Fatalf("owner %d result update %d = {message:%+v pts:%d pts_count:%d}, want only its outgoing message to %d at pts %d", ownerID, i, message, update.Pts, update.PtsCount, peerID, wantPts)
			}
			newMessages++
		case *tg.UpdateMessageID:
			if update.RandomID != randomID || update.ID != wantID {
				t.Fatalf("owner %d result update %d = %+v, want random_id %d mapped to message %d", ownerID, i, update, randomID, wantID)
			}
			messageIDs++
		default:
			t.Fatalf("owner %d result update %d = %T, want only its new-message and message-id updates", ownerID, i, class)
		}
	}
	if newMessages != 1 || messageIDs != 1 {
		t.Fatalf("owner %d result has %d new messages and %d message-id mappings, want one each", ownerID, newMessages, messageIDs)
	}
}

func assertSameAuthKey(t *testing.T, ctx context.Context, st *store.Store, userID int64, want []store.AuthKey) {
	t.Helper()
	got, err := st.AuthKeysByUser(ctx, userID)
	if err != nil {
		t.Fatalf("auth keys for user %d: %v", userID, err)
	}
	if len(got) != len(want) {
		t.Fatalf("user %d auth key count = %d, want %d", userID, len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].UserID != want[i].UserID {
			t.Fatalf("user %d auth key changed: before=%+v after=%+v", userID, want[i], got[i])
		}
	}
}

func onlyDifferenceMessage(t *testing.T, diff *tg.UpdatesDifference, text string, peerID int64) (*tg.Message, bool) {
	t.Helper()
	var found *tg.Message
	for _, class := range diff.NewMessages {
		message, ok := class.(*tg.Message)
		if !ok || message.Message != text {
			continue
		}
		peer, ok := message.PeerID.(*tg.PeerUser)
		if !ok || peer.UserID != peerID || message.Out {
			return nil, false
		}
		if found != nil {
			return nil, false
		}
		found = message
	}
	return found, found != nil
}

type serverWriteGateListener struct {
	net.Listener

	gate *serverWriteGate
}

func (l *serverWriteGateListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &serverWriteGateConn{Conn: conn, gate: l.gate}, nil
}

type serverWriteGateConn struct {
	net.Conn

	gate *serverWriteGate
}

func (c *serverWriteGateConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if attempt := c.gate.claim(); attempt != nil {
		close(attempt.entered)
		<-attempt.decided
		if attempt.drop {
			if err := c.Close(); err != nil {
				return 0, errors.Join(net.ErrClosed, fmt.Errorf("close response-loss socket: %w", err))
			}
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

type serverWriteGate struct {
	mu     sync.Mutex
	active *serverWriteAttempt
}

type serverWriteAttempt struct {
	entered chan struct{}
	decided chan struct{}
	once    sync.Once
	drop    bool
}

func (g *serverWriteGate) arm() (*serverWriteAttempt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != nil {
		return nil, errors.New("server write gate already armed")
	}
	attempt := &serverWriteAttempt{entered: make(chan struct{}), decided: make(chan struct{})}
	g.active = attempt
	return attempt, nil
}

func (g *serverWriteGate) claim() *serverWriteAttempt {
	g.mu.Lock()
	defer g.mu.Unlock()
	attempt := g.active
	g.active = nil
	return attempt
}

func (a *serverWriteAttempt) decide(drop bool) {
	a.once.Do(func() {
		a.drop = drop
		close(a.decided)
	})
}
