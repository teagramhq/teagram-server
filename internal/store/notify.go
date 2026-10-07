package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Postgres LISTEN/NOTIFY channels used for cross-replica update delivery.
const (
	// ChannelUpdates carries user update nudges and persisted transient updates.
	// Payloads are a user id, a sender-suppression tuple, a channel-membership
	// update, a chat-admin event, or a channel-scoped poll-vote update.
	ChannelUpdates = "tg_updates"
	ChannelTyping  = "tg_typing"       // payload: legacy user pair or peer id, sender id, peer type, and action
	ChannelEvict   = "tg_evict"        // payload: "<userID>|<authKeyID>"
	ChannelPost    = "tg_channel_post" // payload: "<channelID>"
	// ChannelEncryption carries a secret-chat state change to one party.
	// payload: "<userID>|<chatID>". It is transient like ChannelTyping: no pts
	// is spent on key exchange, so nothing backfills a missed one until the qts
	// stream lands (MAIN-140).
	ChannelEncryption = "tg_encryption" // payload: "<userID>|<chatID>"
	ChannelStatus     = "tg_status"     // payload: "<userID>|<1 or 0>"
	// ChannelEncryptedMsg carries one encrypted message event to its recipient.
	// payload: "<recipientID>|<qts>". The handler fetches the full event from
	// encrypted_events and pushes updateNewEncryptedMessage.
	ChannelEncryptedMsg = "tg_encrypted_msg" // payload: "<recipientID>|<qts>"
	// ChannelReactions carries a reaction change to all parties holding a message.
	// payload: "<ownerID>|<localID>|<userID>". The handler pushes
	// updateMessageReactions (transient, no pts, same model as updateUserStatus).
	ChannelReactions = "tg_reactions"
	// ChannelPinned carries a pin/unpin change to all members of a chat or channel.
	// payload: "c<peerID>[|<msgID>]" for chat, "h<peerID>[|<msgID>]" for channel.
	// msgID is present on pin (nonzero), absent on unpin. The handler pushes
	// updatePinnedMessages (transient, no pts, same model as reactions).
	ChannelPinned = "tg_pinned"
	// ChannelDialogFilters carries only a folder owner's user id.
	ChannelDialogFilters = "tg_dialog_filters"
	// ChannelDialogPins carries only a default-folder pin owner's user id.
	ChannelDialogPins = "tg_dialog_pins"
)

const channelMembershipPayloadPrefix = "channel_membership|"
const chatAdminPayloadPrefix = "chat_admin|"
const channelPollVotePayloadPrefix = "channel_poll_vote|"
const cloudDraftPayloadPrefix = "cloud_draft|"

type notificationAcceptedAtKey struct{}

type suppressedUpdateKey struct{}

type channelMembershipUpdateKey struct{}
type chatAdminUpdateKey struct{}
type channelPollVoteUpdateKey struct{}
type cloudDraftUpdateKey struct{}
type typingEventContextKey struct{}

// TypingEvent identifies the peer and TL action carried by a typing
// notification. Action is the encoded SendMessageAction constructor and body.
type TypingEvent struct {
	PeerType PeerType
	PeerID   int64
	Action   []byte
}

// WithTypingEvent attaches the decoded transient typing event to a listener
// callback context without expanding the listener callback signature.
func WithTypingEvent(ctx context.Context, event TypingEvent) context.Context {
	event.Action = append([]byte(nil), event.Action...)
	return context.WithValue(ctx, typingEventContextKey{}, event)
}

// TypingEventFromContext returns the event carried by a listener callback.
func TypingEventFromContext(ctx context.Context) (TypingEvent, bool) {
	if ctx == nil {
		return TypingEvent{}, false
	}
	event, ok := ctx.Value(typingEventContextKey{}).(TypingEvent)
	if !ok {
		return TypingEvent{}, false
	}
	event.Action = append([]byte(nil), event.Action...)
	return event, true
}

type chatAdminUpdate struct {
	chatID  int64
	eventID int64
}

type channelPollVoteUpdate struct {
	channelID int64
	pollID    int64
}

// WithCloudDraftUpdate marks an owner-scoped cloud draft notification. The
// payload carries only this peer key; delivery resolves the current value from
// storage after coalescing.
func WithCloudDraftUpdate(ctx context.Context, peer PeerDialogKey) context.Context {
	return context.WithValue(ctx, cloudDraftUpdateKey{}, peer)
}

// CloudDraftUpdateFromContext returns the peer key carried by a cloud draft
// notification.
func CloudDraftUpdateFromContext(ctx context.Context) (PeerDialogKey, bool) {
	if ctx == nil {
		return PeerDialogKey{}, false
	}
	peer, ok := ctx.Value(cloudDraftUpdateKey{}).(PeerDialogKey)
	if !ok || peer.PeerID <= 0 || peer.PeerType < PeerTypeUser || peer.PeerType > PeerTypeChannel {
		return PeerDialogKey{}, false
	}
	return peer, true
}

// CloudDraftNotificationPayload encodes an owner and peer key without private
// draft content for PostgreSQL NOTIFY.
func CloudDraftNotificationPayload(ownerID int64, peer PeerDialogKey) string {
	return cloudDraftPayloadPrefix + pairPayload(ownerID, int64(peer.PeerType)) + "|" + strconv.FormatInt(peer.PeerID, 10)
}

// WithChannelPollVoteUpdate marks a channel-scoped transient poll result.
func WithChannelPollVoteUpdate(ctx context.Context, channelID, pollID int64) context.Context {
	return context.WithValue(ctx, channelPollVoteUpdateKey{}, channelPollVoteUpdate{
		channelID: channelID,
		pollID:    pollID,
	})
}

// ChannelPollVoteUpdateFromContext returns the channel and poll ids carried by
// a channel-scoped transient vote notification.
func ChannelPollVoteUpdateFromContext(ctx context.Context) (channelID, pollID int64, ok bool) {
	if ctx == nil {
		return 0, 0, false
	}
	update, ok := ctx.Value(channelPollVoteUpdateKey{}).(channelPollVoteUpdate)
	if !ok || update.channelID <= 0 || update.pollID <= 0 {
		return 0, 0, false
	}
	return update.channelID, update.pollID, true
}

// SuppressedUpdate identifies the sendMessage update that will be returned by
// an RPC result to one authenticated key. It is carried only to the in-process
// delivery callback; the event remains in the owner's event log.
type SuppressedUpdate struct {
	AuthKeyID int64
	Pts       int
}

// WithSuppressedUpdate carries the RPC-result update identity to the delivery
// callback for this notification.
func WithSuppressedUpdate(ctx context.Context, update SuppressedUpdate) context.Context {
	return context.WithValue(ctx, suppressedUpdateKey{}, update)
}

// SuppressedUpdateFromContext returns the optional RPC-result update identity.
func SuppressedUpdateFromContext(ctx context.Context) (SuppressedUpdate, bool) {
	if ctx == nil {
		return SuppressedUpdate{}, false
	}
	update, ok := ctx.Value(suppressedUpdateKey{}).(SuppressedUpdate)
	if !ok || update.AuthKeyID == 0 || update.Pts <= 0 {
		return SuppressedUpdate{}, false
	}
	return update, true
}

// WithChannelMembershipUpdate marks a user-update notification as a prompt to
// push the named channel's membership view to that user.
func WithChannelMembershipUpdate(ctx context.Context, channelID int64) context.Context {
	return context.WithValue(ctx, channelMembershipUpdateKey{}, channelID)
}

// ChannelMembershipUpdateFromContext returns the channel id carried by a
// membership notification, if this delivery is one.
func ChannelMembershipUpdateFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	channelID, ok := ctx.Value(channelMembershipUpdateKey{}).(int64)
	return channelID, ok && channelID > 0
}

// WithChatAdminUpdate marks an updates notification as a prompt to push one
// persisted, transient basic-chat administrator update.
func WithChatAdminUpdate(ctx context.Context, chatID, eventID int64) context.Context {
	return context.WithValue(ctx, chatAdminUpdateKey{}, chatAdminUpdate{
		chatID: chatID, eventID: eventID,
	})
}

// ChatAdminUpdateFromContext returns the chat and event ids carried by a
// transient update notification, if this delivery is one.
func ChatAdminUpdateFromContext(ctx context.Context) (chatID, eventID int64, ok bool) {
	if ctx == nil {
		return 0, 0, false
	}
	update, ok := ctx.Value(chatAdminUpdateKey{}).(chatAdminUpdate)
	return update.chatID, update.eventID, ok && update.chatID > 0 && update.eventID > 0
}

// WithNotificationAcceptedAt carries a valid tg_updates acceptance timestamp
// to the in-process delivery callback. It never leaves the process or enters a
// database, update payload, log, or protocol response.
func WithNotificationAcceptedAt(ctx context.Context, acceptedAt time.Time) context.Context {
	return context.WithValue(ctx, notificationAcceptedAtKey{}, acceptedAt)
}

// NotificationAcceptedAt returns the acceptance timestamp carried by a valid
// tg_updates callback context.
func NotificationAcceptedAt(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	acceptedAt, ok := ctx.Value(notificationAcceptedAtKey{}).(time.Time)
	if !ok || acceptedAt.IsZero() {
		return time.Time{}, false
	}
	return acceptedAt, true
}

// Notify emits a Postgres NOTIFY on channel with payload. It is the cross-replica
// nudge that wakes each process's Listener after an event transaction commits.
func (s *Store) Notify(ctx context.Context, channel, payload string) error {
	// Bound pool acquisition and NOTIFY execution for this attempt. Post-commit
	// callers detach once around the whole fan-out, so every recipient shares the
	// same deadline instead of extending it by five seconds per recipient.
	notifyCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	if _, err := s.pool.Exec(notifyCtx, `SELECT pg_notify($1, $2)`, channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}

const notificationTimeout = 5 * time.Second

// NotificationContext detaches a committed notification operation from its RPC
// while bounding it to one shared five-second budget.
func NotificationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), notificationTimeout)
}

// Reconnect pacing for the listener loop. A database that stays down, or one
// that flaps, is retried on a growing delay and never in a tight loop.
const (
	listenerBackoffMin = 100 * time.Millisecond
	listenerBackoffMax = 30 * time.Second
	// listenerStableFor is how long a connection must survive for its failure
	// to count as a fresh one. Below it the database is still unhealthy, so the
	// delay keeps growing; above it the loop starts over at the floor, whether
	// or not that connection ever carried a notification.
	listenerStableFor = 30 * time.Second
)

// nextBackoff returns the delay before the next reconnect attempt: the floor
// when the connection that just failed had been up long enough to count as
// healthy, otherwise the previous delay doubled up to the cap. uptime is zero
// for an attempt that never connected.
func nextBackoff(prev, uptime time.Duration) time.Duration {
	if uptime >= listenerStableFor {
		return listenerBackoffMin
	}
	return min(2*prev, listenerBackoffMax)
}

// Listener runs LISTEN on a dedicated pgx connection and dispatches each
// notification to the delivery callbacks, reconnecting when that connection
// breaks.
type Listener struct {
	log       *slog.Logger
	scheduler *notificationScheduler
	// metrics is the only recorder accepted by StartListener. Its fixed
	// in-process operation runs before the delivery callback.
	metrics *NotificationMetrics
	// These overrides exist only in package tests to inject recorder outcomes;
	// production leaves them nil and uses the fixed metrics recorder above.
	validNotificationRecorder   func(channel string) error
	invalidNotificationRecorder func() error

	// closeErr is the loop's final connection close error. The loop goroutine
	// writes it before returning and stop reads it after wg.Wait, so the
	// WaitGroup orders the two accesses: the connection needs no lock because
	// only the loop goroutine ever touches it.
	closeErr error
}

// ErrListenerStopped is the cancellation cause given to callbacks interrupted
// by the listener's returned stop function.
var ErrListenerStopped = errors.New("notification listener stopped")

// StartListener opens a dedicated connection, subscribes to the update, typing,
// evict, channel-post, encryption, status, encrypted-message, reactions, and
// pinned channels, and runs the notification loop until the returned stop
// function is called (which cancels the loop, drains it, and returns the error
// from closing the connection). deliver receives a userID whose events changed;
// typing receives (peerID, fromUserID) with the peer kind and action in the
// callback context;
// evict receives (userID, authKeyID) for a session revoked on any replica;
// channelPost receives a channelID whose post was just committed; encryption
// receives (userID, chatID) for a secret-chat state change; status receives
// (userID, online) for a status change; encryptedMsg receives (recipientID, qts)
// for a secret-chat message; reactions receives (ownerID, localID, userID) for
// a reaction change on a specific message copy; pinned receives (peerType, peerID, pinnedMsgID) for a pin/unpin in a chat or channel; pinnedMsgID is nonzero on pin, zero on unpin.
// An optional built-in metrics recorder receives fixed-channel valid counts and
// aggregate malformed counts inline before delivery callbacks. StartListener
// does not accept arbitrary telemetry sinks: the recorder's operation is fixed
// and process-local, so delivery and shutdown cannot depend on an external
// execution bound.
//
// A broken connection is reconnected with bounded backoff rather than ending
// delivery for the life of the process. Notifications emitted while the
// listener is reconnecting are lost, which push already tolerates: the client's
// next getDifference backfills them.
//
// The LISTEN goroutine only parses and schedules callbacks. A fixed worker pool
// runs them, and notifications for the same routing key remain serial while a
// blocked key cannot occupy notification ingestion or every worker.
func StartListener(
	ctx context.Context,
	dsn string,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	log *slog.Logger,
	notifyMetrics ...*NotificationMetrics,
) (*Listener, func() error, error) {
	return startListener(ctx, dsn, deliver, typing, evict, channelPost, encryption, status, encryptedMsg, reactions, pinned, nil, nil, nil, log, notifyMetrics...)
}

// StartListenerWithDialogFilters adds private-folder invalidation and listener
// reconnect callbacks for the server process.
func StartListenerWithDialogFilters(
	ctx context.Context,
	dsn string,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	dialogFilters func(ctx context.Context, ownerID int64),
	reconnected func(),
	log *slog.Logger,
	notifyMetrics ...*NotificationMetrics,
) (*Listener, func() error, error) {
	return startListener(ctx, dsn, deliver, typing, evict, channelPost, encryption, status, encryptedMsg, reactions, pinned, dialogFilters, nil, reconnected, log, notifyMetrics...)
}

// StartListenerWithDialogPins adds owner-scoped pin notifications alongside
// private-folder invalidation and listener reconnect callbacks.
func StartListenerWithDialogPins(
	ctx context.Context,
	dsn string,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	dialogFilters func(ctx context.Context, ownerID int64),
	dialogPins func(ctx context.Context, ownerID int64),
	reconnected func(),
	log *slog.Logger,
	notifyMetrics ...*NotificationMetrics,
) (*Listener, func() error, error) {
	return startListener(ctx, dsn, deliver, typing, evict, channelPost, encryption, status, encryptedMsg, reactions, pinned, dialogFilters, dialogPins, reconnected, log, notifyMetrics...)
}

func startListener(
	ctx context.Context,
	dsn string,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	dialogFilters func(ctx context.Context, ownerID int64),
	dialogPins func(ctx context.Context, ownerID int64),
	reconnected func(),
	log *slog.Logger,
	notifyMetrics ...*NotificationMetrics,
) (*Listener, func() error, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	conn, err := connectAndListen(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}

	var metrics *NotificationMetrics
	if len(notifyMetrics) > 0 {
		metrics = notifyMetrics[0]
	}
	loopCtx, cancel := context.WithCancelCause(ctx)
	l := &Listener{log: log, metrics: metrics, scheduler: newNotificationScheduler(loopCtx)}
	var wg sync.WaitGroup
	wg.Go(func() {
		l.run(loopCtx, conn, dsn, deliver, typing, evict, channelPost, encryption, status, encryptedMsg, reactions, pinned, dialogFilters, dialogPins, reconnected)
	})

	stop := func() error {
		cancel(ErrListenerStopped)
		wg.Wait()
		l.scheduler.stop()
		return l.closeErr
	}
	return l, stop, nil
}

// connectAndListen opens a connection subscribed to every channel. They are
// always subscribed together: a connection carrying only some of them silently
// drops a whole class of delivery.
func connectAndListen(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("listener connect: %w", err)
	}
	for _, ch := range []string{ChannelUpdates, ChannelTyping, ChannelEvict, ChannelPost, ChannelEncryption, ChannelStatus, ChannelEncryptedMsg, ChannelReactions, ChannelPinned, ChannelDialogFilters, ChannelDialogPins} {
		// ch is a constant channel identifier, never user input (no injection).
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			_ = conn.Close(ctx) //nolint:errcheck // best-effort close on setup failure
			return nil, fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	return conn, nil
}

// run dispatches notifications until ctx is canceled, reconnecting whenever the
// connection breaks. The loop goroutine owns conn exclusively — it is the only
// thing that replaces or closes it — so a reconnect needs no lock and no lock
// ordering against writeMu or the session registry.
func (l *Listener) run(
	ctx context.Context,
	conn *pgx.Conn,
	dsn string,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	dialogFilters func(ctx context.Context, ownerID int64),
	dialogPins func(ctx context.Context, ownerID int64),
	reconnected func(),
) {
	backoff := listenerBackoffMin
	for {
		if conn == nil {
			if !sleepCtx(ctx, backoff) {
				return // canceled mid-backoff: nothing open to close
			}
			c, err := connectAndListen(ctx, dsn)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				backoff = nextBackoff(backoff, 0)
				l.log.Error("listener reconnect", "err", err, "retry_in", backoff)
				continue
			}
			l.log.Info("listener reconnected")
			if reconnected != nil {
				reconnected()
			}
			conn = c
		}

		up := time.Now()
		err := l.dispatch(ctx, conn, deliver, typing, evict, channelPost, encryption, status, encryptedMsg, reactions, pinned, dialogFilters, dialogPins)
		closeErr := conn.Close(context.Background())
		conn = nil
		if ctx.Err() != nil {
			l.closeErr = closeErr
			return // canceled: clean shutdown
		}
		backoff = nextBackoff(backoff, time.Since(up))
		l.log.Error("listener connection lost", "err", err, "retry_in", backoff)
	}
}

// dispatch consumes notifications on conn until ctx is canceled or the
// connection fails, returning the error that ended it.
func (l *Listener) dispatch(
	ctx context.Context,
	conn *pgx.Conn,
	deliver func(ctx context.Context, userID int64),
	typing func(ctx context.Context, peerID, fromID int64),
	evict func(ctx context.Context, userID, authKeyID int64),
	channelPost func(ctx context.Context, channelID int64),
	encryption func(ctx context.Context, userID, chatID int64),
	status func(ctx context.Context, userID int64, online bool),
	encryptedMsg func(ctx context.Context, recipientID int64, qts int),
	reactions func(ctx context.Context, ownerID, localID, userID int64),
	pinned func(ctx context.Context, peerType PeerType, peerID int64, pinnedMsgID int32),
	dialogFilters func(ctx context.Context, ownerID int64),
	dialogPins func(ctx context.Context, ownerID int64),
) error {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch n.Channel {
		case ChannelUpdates:
			if strings.HasPrefix(n.Payload, cloudDraftPayloadPrefix) {
				ownerID, peer, perr := parseCloudDraftPayload(n.Payload)
				if perr != nil {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_updates cloud draft payload")
					continue
				}
				l.recordValidNotification(ChannelUpdates)
				routeKey := "cloud-draft:" + strconv.FormatInt(ownerID, 10) + ":" + strconv.Itoa(int(peer.PeerType)) + ":" + strconv.FormatInt(peer.PeerID, 10)
				l.schedule(routeKey, notificationTask{
					ctx:      WithCloudDraftUpdate(ctx, peer),
					coalesce: true,
					run: func(ctx context.Context) {
						deliver(ctx, ownerID)
					},
				})
				continue
			}
			if strings.HasPrefix(n.Payload, channelPollVotePayloadPrefix) {
				channelID, pollID, perr := parseChannelPollVotePayload(n.Payload)
				if perr != nil {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_updates channel poll vote payload")
					continue
				}
				l.recordValidNotification(ChannelUpdates)
				l.schedule("channel-poll-vote:"+strconv.FormatInt(channelID, 10)+":"+strconv.FormatInt(pollID, 10), notificationTask{
					ctx:      ctx,
					coalesce: true,
					run: func(ctx context.Context) {
						deliver(WithChannelPollVoteUpdate(ctx, channelID, pollID), 0)
					},
				})
				continue
			}
			if strings.HasPrefix(n.Payload, channelMembershipPayloadPrefix) {
				userID, channelID, perr := parseChannelMembershipPayload(n.Payload)
				if perr != nil {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_updates membership payload")
					continue
				}
				l.recordValidNotification(ChannelUpdates)
				l.schedule("channel-membership:"+strconv.FormatInt(userID, 10)+":"+strconv.FormatInt(channelID, 10), notificationTask{
					ctx: WithChannelMembershipUpdate(ctx, channelID),
					run: func(ctx context.Context) {
						deliver(ctx, userID)
					},
				})
				continue
			}
			if strings.HasPrefix(n.Payload, chatAdminPayloadPrefix) {
				chatID, eventID, perr := parseChatAdminPayload(n.Payload)
				if perr != nil {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_updates chat admin payload")
					continue
				}
				l.recordValidNotification(ChannelUpdates)
				l.schedule("chat-admin:"+strconv.FormatInt(chatID, 10), notificationTask{
					ctx: WithChatAdminUpdate(ctx, chatID, eventID),
					run: func(ctx context.Context) {
						deliver(ctx, 0)
					},
				})
				continue
			}
			userID, update, perr := parseUpdatesPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_updates payload", "payload", n.Payload)
				continue
			}
			acceptedAt := time.Now()
			l.recordValidNotification(ChannelUpdates)
			deliveryCtx := WithNotificationAcceptedAt(ctx, acceptedAt)
			if update.AuthKeyID != 0 {
				deliveryCtx = WithSuppressedUpdate(deliveryCtx, update)
			}
			l.schedule("updates:"+strconv.FormatInt(userID, 10), notificationTask{
				ctx:      deliveryCtx,
				coalesce: update.AuthKeyID == 0,
				run: func(ctx context.Context) {
					deliver(ctx, userID)
				},
			})
		case ChannelTyping:
			peerID, fromID, event, perr := parseTypingPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_typing payload", "err", perr)
				continue
			}
			l.recordValidNotification(ChannelTyping)
			l.schedule("typing:"+strconv.Itoa(int(event.PeerType))+":"+strconv.FormatInt(peerID, 10)+":"+strconv.FormatInt(fromID, 10), notificationTask{
				ctx:      WithTypingEvent(ctx, event),
				coalesce: true,
				run: func(ctx context.Context) {
					typing(ctx, peerID, fromID)
				},
			})
		case ChannelEvict:
			userID, authKeyID, perr := parsePairPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				// Dropped, never widened: an evict that cannot be read must not
				// escalate into closing every connection of some user.
				l.log.Warn("bad tg_evict payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelEvict)
			// Eviction closes a matching transport without taking writeMu. Keep it
			// on the listener goroutine so revocation cannot wait behind saturated
			// push workers or be dropped by their bounded queue.
			evict(ctx, userID, authKeyID)
		case ChannelPost:
			channelID, perr := strconv.ParseInt(n.Payload, 10, 64)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_channel_post payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelPost)
			l.schedule("post:"+strconv.FormatInt(channelID, 10), notificationTask{
				ctx:      ctx,
				coalesce: true,
				run: func(ctx context.Context) {
					channelPost(ctx, channelID)
				},
			})
		case ChannelEncryption:
			userID, chatID, perr := parsePairPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_encryption payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelEncryption)
			l.schedule("encryption:"+strconv.FormatInt(userID, 10), notificationTask{
				ctx: ctx,
				run: func(ctx context.Context) {
					encryption(ctx, userID, chatID)
				},
			})
		case ChannelStatus:
			userID, onlineID, perr := parsePairPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_status payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelStatus)
			online := onlineID == 1
			l.schedule("status:"+strconv.FormatInt(userID, 10), notificationTask{
				ctx:      ctx,
				coalesce: true,
				run: func(ctx context.Context) {
					status(ctx, userID, online)
				},
			})
		case ChannelEncryptedMsg:
			recipientID, qts64, perr := parsePairPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_encrypted_msg payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelEncryptedMsg)
			l.schedule("encrypted-msg:"+strconv.FormatInt(recipientID, 10), notificationTask{
				ctx: ctx,
				run: func(ctx context.Context) {
					encryptedMsg(ctx, recipientID, int(qts64))
				},
			})
		case ChannelReactions:
			opts, perr := parseReactionPayload(n.Payload)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_reactions payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelReactions)
			l.schedule("reactions:"+strconv.FormatInt(opts.userID, 10)+":"+strconv.FormatInt(opts.localID, 10), notificationTask{
				ctx:      ctx,
				coalesce: true,
				run: func(ctx context.Context) {
					reactions(ctx, opts.ownerID, opts.localID, opts.userID)
				},
			})
		case ChannelPinned:
			payload := n.Payload
			if len(payload) < 2 {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_pinned payload", "payload", n.Payload)
				continue
			}
			var peerType PeerType
			switch payload[0] {
			case 'c':
				peerType = PeerTypeChat
			case 'h':
				peerType = PeerTypeChannel
			default:
				l.recordInvalidNotification()
				l.log.Warn("bad tg_pinned peer type", "payload", n.Payload)
				continue
			}
			payload = payload[1:]
			var pinnedMsgID int32
			if strings.HasPrefix(payload, "-") {
				// Unpin: "-<peerID>"
				payload = payload[1:]
			} else {
				// Pin: "<peerID>|<msgID>"
				parts := strings.SplitN(payload, "|", 2)
				if len(parts) != 2 {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_pinned payload", "payload", n.Payload)
					continue
				}
				msgID, perr := strconv.ParseInt(parts[1], 10, 32)
				if perr != nil {
					l.recordInvalidNotification()
					l.log.Warn("bad tg_pinned msgID", "payload", n.Payload)
					continue
				}
				pinnedMsgID = int32(msgID)
				payload = parts[0]
			}
			peerID, perr := strconv.ParseInt(payload, 10, 64)
			if perr != nil {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_pinned payload", "payload", n.Payload)
				continue
			}
			l.recordValidNotification(ChannelPinned)
			l.schedule("pinned:"+strconv.FormatInt(int64(peerType), 10)+":"+strconv.FormatInt(peerID, 10), notificationTask{
				ctx:      ctx,
				coalesce: true,
				run: func(ctx context.Context) {
					pinned(ctx, peerType, peerID, pinnedMsgID)
				},
			})
		case ChannelDialogFilters:
			ownerID, perr := strconv.ParseInt(n.Payload, 10, 64)
			if perr != nil || ownerID <= 0 {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_dialog_filters payload")
				continue
			}
			l.recordValidNotification(ChannelDialogFilters)
			if dialogFilters != nil {
				// The callback only advances in-memory recovery state and never
				// waits for the worker pool or a socket write.
				dialogFilters(ctx, ownerID)
			}
		case ChannelDialogPins:
			ownerID, perr := strconv.ParseInt(n.Payload, 10, 64)
			if perr != nil || ownerID <= 0 {
				l.recordInvalidNotification()
				l.log.Warn("bad tg_dialog_pins payload")
				continue
			}
			l.recordValidNotification(ChannelDialogPins)
			if dialogPins != nil {
				l.schedule("dialog-pins:"+strconv.FormatInt(ownerID, 10), notificationTask{
					ctx:      ctx,
					coalesce: true,
					run: func(ctx context.Context) {
						dialogPins(ctx, ownerID)
					},
				})
			}
		default:
			l.recordInvalidNotification()
		}
	}
}

func parseUpdatesPayload(payload string) (int64, SuppressedUpdate, error) {
	parts := strings.Split(payload, "|")
	if len(parts) != 1 && len(parts) != 3 {
		return 0, SuppressedUpdate{}, errors.New("invalid update payload fields")
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		return 0, SuppressedUpdate{}, errors.New("invalid update owner")
	}
	if len(parts) == 1 {
		return userID, SuppressedUpdate{}, nil
	}
	authKeyID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || authKeyID == 0 {
		return 0, SuppressedUpdate{}, errors.New("invalid update auth key")
	}
	pts, err := strconv.Atoi(parts[2])
	if err != nil || pts <= 0 {
		return 0, SuppressedUpdate{}, errors.New("invalid update pts")
	}
	return userID, SuppressedUpdate{AuthKeyID: authKeyID, Pts: pts}, nil
}

func parseCloudDraftPayload(payload string) (int64, PeerDialogKey, error) {
	if !strings.HasPrefix(payload, cloudDraftPayloadPrefix) {
		return 0, PeerDialogKey{}, errors.New("invalid cloud draft payload prefix")
	}
	parts := strings.Split(strings.TrimPrefix(payload, cloudDraftPayloadPrefix), "|")
	if len(parts) != 3 {
		return 0, PeerDialogKey{}, errors.New("invalid cloud draft payload fields")
	}
	ownerID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || ownerID <= 0 {
		return 0, PeerDialogKey{}, errors.New("invalid cloud draft owner")
	}
	peerType, err := strconv.ParseInt(parts[1], 10, 16)
	if err != nil || peerType < int64(PeerTypeUser) || peerType > int64(PeerTypeChannel) {
		return 0, PeerDialogKey{}, errors.New("invalid cloud draft peer type")
	}
	peerID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || peerID <= 0 {
		return 0, PeerDialogKey{}, errors.New("invalid cloud draft peer id")
	}
	return ownerID, PeerDialogKey{PeerType: PeerType(peerType), PeerID: peerID}, nil
}

func parseChannelMembershipPayload(payload string) (int64, int64, error) {
	if !strings.HasPrefix(payload, channelMembershipPayloadPrefix) {
		return 0, 0, errors.New("invalid channel membership payload prefix")
	}
	userID, channelID, err := parsePairPayload(strings.TrimPrefix(payload, channelMembershipPayloadPrefix))
	if err != nil || userID <= 0 || channelID <= 0 {
		return 0, 0, errors.New("invalid channel membership payload")
	}
	return userID, channelID, nil
}

func parseChatAdminPayload(payload string) (int64, int64, error) {
	if !strings.HasPrefix(payload, chatAdminPayloadPrefix) {
		return 0, 0, errors.New("invalid chat admin payload prefix")
	}
	chatID, eventID, err := parsePairPayload(strings.TrimPrefix(payload, chatAdminPayloadPrefix))
	if err != nil || chatID <= 0 || eventID <= 0 {
		return 0, 0, errors.New("invalid chat admin payload")
	}
	return chatID, eventID, nil
}

func parseChannelPollVotePayload(payload string) (int64, int64, error) {
	if !strings.HasPrefix(payload, channelPollVotePayloadPrefix) {
		return 0, 0, errors.New("invalid channel poll vote payload prefix")
	}
	channelID, pollID, err := parsePairPayload(strings.TrimPrefix(payload, channelPollVotePayloadPrefix))
	if err != nil || channelID <= 0 || pollID <= 0 {
		return 0, 0, errors.New("invalid channel poll vote payload")
	}
	return channelID, pollID, nil
}

// recordValidNotification isolates the listener from recorder failures. A
// recorder is telemetry only: an error or panic must not alter delivery.
func (l *Listener) recordValidNotification(channel string) {
	if l.metrics == nil && l.validNotificationRecorder == nil {
		return
	}
	var failed bool
	if l.validNotificationRecorder != nil {
		failed = InvokeRecorder(func() error { return l.validNotificationRecorder(channel) })
	} else {
		failed = InvokeRecorder(func() error {
			return l.metrics.RecordValidNotification(channel)
		})
	}
	if failed {
		ReportRecorderFailure(l.log, l.metrics, RecorderFailureNotification)
	}
}

// recordInvalidNotification isolates malformed-input accounting from the
// listener. Invalid notifications carry no channel or payload to telemetry.
func (l *Listener) recordInvalidNotification() {
	if l.metrics == nil && l.invalidNotificationRecorder == nil {
		return
	}
	var failed bool
	if l.invalidNotificationRecorder != nil {
		failed = InvokeRecorder(func() error { return l.invalidNotificationRecorder() })
	} else {
		failed = InvokeRecorder(func() error {
			return l.metrics.RecordInvalidNotification()
		})
	}
	if failed {
		ReportRecorderFailure(l.log, l.metrics, RecorderFailureNotification)
	}
}

// sleepCtx waits d, reporting false if ctx ended first. Shutdown must not have
// to wait out a backoff.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ChannelPostPayload formats a tg_channel_post NOTIFY payload from channelID.
func ChannelPostPayload(channelID int64) string {
	return strconv.FormatInt(channelID, 10)
}

// ChannelMembershipPayload formats a tg_updates notification that pushes
// channelID's viewer-specific membership view to userID.
func ChannelMembershipPayload(userID, channelID int64) string {
	return channelMembershipPayloadPrefix + pairPayload(userID, channelID)
}

// ChatAdminPayload formats a tg_updates notification naming one persisted,
// transient chat-admin update event.
func ChatAdminPayload(chatID, eventID int64) string {
	return chatAdminPayloadPrefix + pairPayload(chatID, eventID)
}

// ChannelPollVotePayload formats a tg_updates notification for one channel's
// poll results. Each replica fans it out only to connected, unbanned members.
func ChannelPollVotePayload(channelID, pollID int64) string {
	return channelPollVotePayloadPrefix + pairPayload(channelID, pollID)
}

// EncryptionPayload formats a tg_encryption NOTIFY payload naming the party to
// notify and the secret chat whose state changed.
func EncryptionPayload(userID, chatID int64) string {
	return pairPayload(userID, chatID)
}

// EncryptedMsgPayload formats a tg_encrypted_msg NOTIFY payload naming the
// recipient and the qts of the event they must push.
func EncryptedMsgPayload(recipientID int64, qts int) string {
	return pairPayload(recipientID, int64(qts))
}

// TypingPayload formats the legacy two-id user typing notification.
func TypingPayload(peerID, fromID int64) string {
	return pairPayload(peerID, fromID)
}

// TypingEventPayload formats a transient typing notification for a user, chat,
// or channel peer and preserves the encoded SendMessageAction.
func TypingEventPayload(peerType PeerType, peerID, fromID int64, action []byte) string {
	return strconv.FormatInt(peerID, 10) + "|" + strconv.FormatInt(fromID, 10) + "|" +
		strconv.Itoa(int(peerType)) + "|" + base64.RawURLEncoding.EncodeToString(action)
}

// EvictPayload formats a tg_evict NOTIFY payload naming the revoked session: the
// user the deleted auth key was bound to, and the auth key id itself.
func EvictPayload(userID, authKeyID int64) string {
	return pairPayload(userID, authKeyID)
}

// StatusPayload formats a tg_status NOTIFY payload. The second id is 1 for
// online, 0 for offline.
func StatusPayload(userID int64, online bool) string {
	second := "0"
	if online {
		second = "1"
	}
	return strconv.FormatInt(userID, 10) + "|" + second
}

// ReactionPayload formats a tg_reactions NOTIFY payload carrying the
// owner, local message id, and the target user to push to.
// Payload: "ownerID|localID|userID".
func ReactionPayload(ownerID, localID, userID int64) string {
	return strconv.FormatInt(ownerID, 10) + "|" +
		strconv.FormatInt(localID, 10) + "|" +
		strconv.FormatInt(userID, 10)
}

// PinnedPayload formats a tg_pinned NOTIFY payload naming the peer whose
// pinned message changed. Payload format: "c<peerID>|<msgID>" for chat pin,
// "c-<peerID>" for chat unpin, "h<peerID>|<msgID>" for channel pin, "h-<peerID>"
// for channel unpin. The leading letter disambiguates chat vs channel so
// DeliverPinned does not need to probe both tables. pinnedMsgID is nonzero
// on pin, zero on unpin.
func PinnedPayload(peerType PeerType, peerID int64, pinnedMsgID int32) string {
	prefix := "c"
	if peerType == PeerTypeChannel {
		prefix = "h"
	}
	if pinnedMsgID != 0 {
		return prefix + strconv.FormatInt(peerID, 10) + "|" + strconv.FormatInt(int64(pinnedMsgID), 10)
	}
	return prefix + "-" + strconv.FormatInt(peerID, 10)
}

// reactionPayload carries the parsed fields of a tg_reactions NOTIFY payload.
type reactionPayload struct {
	ownerID int64
	localID int64
	userID  int64
}

// parseReactionPayload decodes a tg_reactions NOTIFY payload into its three
// components: ownerID, localID, and userID.
func parseReactionPayload(payload string) (reactionPayload, error) {
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return reactionPayload{}, fmt.Errorf("malformed reaction payload %q", payload)
	}
	ownerID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return reactionPayload{}, err
	}
	localID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return reactionPayload{}, err
	}
	userID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return reactionPayload{}, err
	}
	return reactionPayload{ownerID: ownerID, localID: localID, userID: userID}, nil
}

// pairPayload encodes the two-id payload shape shared by the pair channels.
func pairPayload(first, second int64) string {
	return strconv.FormatInt(first, 10) + "|" + strconv.FormatInt(second, 10)
}

func parsePairPayload(payload string) (first, second int64, err error) {
	firstStr, secondStr, ok := strings.Cut(payload, "|")
	if !ok {
		return 0, 0, fmt.Errorf("malformed pair payload %q", payload)
	}
	first, err = strconv.ParseInt(firstStr, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	second, err = strconv.ParseInt(secondStr, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return first, second, nil
}

func parseTypingPayload(payload string) (peerID, fromID int64, event TypingEvent, err error) {
	parts := strings.Split(payload, "|")
	if len(parts) == 2 {
		peerID, fromID, err = parsePairPayload(payload)
		if err != nil {
			return 0, 0, TypingEvent{}, err
		}
		return peerID, fromID, TypingEvent{PeerType: PeerTypeUser, PeerID: peerID}, nil
	}
	if len(parts) != 4 {
		return 0, 0, TypingEvent{}, errors.New("malformed typing payload")
	}
	peerID, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || peerID <= 0 {
		return 0, 0, TypingEvent{}, errors.New("invalid typing peer id")
	}
	fromID, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || fromID <= 0 {
		return 0, 0, TypingEvent{}, errors.New("invalid typing sender id")
	}
	peerType, err := strconv.ParseInt(parts[2], 10, 16)
	if err != nil {
		return 0, 0, TypingEvent{}, errors.New("invalid typing peer type")
	}
	event.PeerType = PeerType(peerType)
	if event.PeerType != PeerTypeUser && event.PeerType != PeerTypeChat && event.PeerType != PeerTypeChannel {
		return 0, 0, TypingEvent{}, errors.New("unsupported typing peer type")
	}
	event.PeerID = peerID
	event.Action, err = base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(event.Action) == 0 {
		return 0, 0, TypingEvent{}, errors.New("invalid typing action")
	}
	return peerID, fromID, event, nil
}

// WaitForNotificationListener blocks until n backends in the current database
// are parked on a ClientRead wait — the pg_stat_activity signature of a
// connection blocked in WaitForNotification. Tests that emit NOTIFY must
// confirm the listener is actually reading first; otherwise the notification
// races the initial WaitForNotification call and is lost.
func WaitForNotificationListener(ctx context.Context, s *Store, n int) error {
	const timeout = 30 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		var got int
		err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Client'
			   AND wait_event = 'ClientRead'`).Scan(&got)
		if err != nil {
			return err
		}
		if got >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s for %d notification listeners, saw %d", timeout, n, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
