package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestExpiredMessageRetriesReturnNoMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	sender := mustUser(t, s, "+15551299011")
	recipient := mustUser(t, s, "+15551299012")
	const randomID = int64(99011)
	req := &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(sender.ID, recipient.ID), Message: "ttl text secret", RandomID: randomID,
	}
	if first, err := api.SendMessageForTest(s, sender.ID, req); err != nil || first == nil {
		t.Fatalf("initial send = (%v, %v), want a message", first, err)
	}
	channelExec(t, ctx, dsn, `UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND random_id = $2`, sender.ID, randomID)

	if result, err := api.SendMessageForTest(s, sender.ID, req); result != nil {
		t.Fatalf("expired text retry returned %T, want no serialized message", result)
	} else {
		rpcError(t, err, "RANDOM_ID_DUPLICATE")
	}

	mediaReq := &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(sender.ID, recipient.ID),
		Message:  "replacement caption",
		Media:    uploadedPhoto(99012, 1, "retry.jpg", "d41d8cd98f00b204e9800998ecf8427e"),
		RandomID: randomID,
	}
	if result, err := api.SendMediaForTest(s, sender.ID, newBlobs(t), api.TestMaxUserStorageBytes, mediaReq); result != nil {
		t.Fatalf("expired media retry returned %T, want no serialized message", result)
	} else {
		rpcError(t, err, "RANDOM_ID_DUPLICATE")
	}
}

func TestExpiredChannelTextRetryReturnsNoPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, _, channel := channelWith(t, s, "+15551299021", "+15551299022")
	const randomID = int64(99021)
	req := &tg.MessagesSendMessageRequest{
		Peer: channelPeer(creator.ID, channel.ID), Message: "ttl channel secret", RandomID: randomID,
	}
	if first, err := api.SendMessageForTest(s, creator.ID, req); err != nil || first == nil {
		t.Fatalf("initial channel send = (%v, %v), want a post", first, err)
	}
	channelExec(t, ctx, dsn, `UPDATE channel_messages SET expires_at = statement_timestamp() - interval '1 second' WHERE channel_id = $1 AND random_id = $2`, channel.ID, randomID)

	if result, err := api.SendMessageForTest(s, creator.ID, req); result != nil {
		t.Fatalf("expired channel retry returned %T, want no serialized post", result)
	} else {
		rpcError(t, err, "RANDOM_ID_DUPLICATE")
	}
}
