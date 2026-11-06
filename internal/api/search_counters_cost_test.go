package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type searchCounterFileFixture struct {
	name          string
	mediaKind     string
	subtypeRights []string
}

func searchCounterFileFixtures() []searchCounterFileFixture {
	return []searchCounterFileFixture{
		{name: "document.bin", mediaKind: "document"},
		{name: "photo.jpg", mediaKind: "photo", subtypeRights: []string{"send_photos"}},
		{name: "video.mp4", mediaKind: "document", subtypeRights: []string{"send_videos"}},
		{name: "animation.mp4", mediaKind: "document", subtypeRights: []string{"send_gifs"}},
		{name: "voice.ogg", mediaKind: "document", subtypeRights: []string{"send_voices"}},
		{name: "round.ogg", mediaKind: "document", subtypeRights: []string{"send_roundvideos"}},
		{name: "audio.mp3", mediaKind: "document", subtypeRights: []string{"send_audios"}},
	}
}

func insertSearchCounterFile(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64, fixture searchCounterFileFixture, localID int64) int64 {
	t.Helper()
	var fileID int64
	err := conn.QueryRow(ctx, `INSERT INTO files (
 uploader_id, access_hash, size, mime_type, file_name, stored, subtype_rights,
 media_kind, width, height
) VALUES ($1, $2, 1, 'application/octet-stream', $3, true, $4, $5,
 CASE WHEN $5 = 'photo' THEN 16 ELSE NULL END,
 CASE WHEN $5 = 'photo' THEN 16 ELSE NULL END)
RETURNING id`, ownerID, 700000000+localID, fixture.name, fixture.subtypeRights, fixture.mediaKind).Scan(&fileID)
	if err != nil {
		t.Fatalf("insert %s search-counter file: %v", fixture.name, err)
	}
	return fileID
}

func seedSearchCounterDialogMedia(t *testing.T, ctx context.Context, conn *pgx.Conn, s *store.Store, ownerID, peerID int64) {
	t.Helper()
	for i, fixture := range searchCounterFileFixtures() {
		localID := int64(10001 + i)
		fileID := insertSearchCounterFile(t, ctx, conn, ownerID, fixture, localID)
		if _, err := conn.Exec(ctx, `INSERT INTO messages (
 owner_id, local_id, peer_id, peer_type, from_id, message, out, file_id
) VALUES ($1, $2, $3, 1, $1, 'counter media fixture', true, $4)`, ownerID, localID, peerID, fileID); err != nil {
			t.Fatalf("insert %s search-counter message: %v", fixture.name, err)
		}
	}

	const pollLocalID = int64(10020)
	const peerLocalID = int64(1)
	if _, err := conn.Exec(ctx, `INSERT INTO messages (
 owner_id, local_id, peer_id, peer_type, from_id, message, out, peer_local_id
) VALUES
 ($1, $2, $3, 1, $1, 'counter poll fixture', true, $4),
 ($3, $4, $1, 1, $1, 'counter poll fixture', false, $2)`, ownerID, pollLocalID, peerID, peerLocalID); err != nil {
		t.Fatalf("insert search-counter poll messages: %v", err)
	}
	_, duplicate, err := s.CreatePoll(ctx, ownerID, store.PollMessageRef{
		PeerType: store.PeerTypeUser, PeerID: peerID, LocalID: pollLocalID,
	}, store.PollDraft{
		Question: []byte("counter poll"),
		Answers: []store.PollAnswer{
			{Option: []byte("yes"), Text: []byte("Yes")},
			{Option: []byte("no"), Text: []byte("No")},
		},
	})
	if err != nil || duplicate {
		t.Fatalf("create search-counter poll: duplicate=%v err=%v", duplicate, err)
	}
}

func seedSearchCounterChannelMedia(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID, channelID int64) {
	t.Helper()
	for i, fixture := range searchCounterFileFixtures() {
		localID := int64(102 + i)
		fileID := insertSearchCounterFile(t, ctx, conn, ownerID, fixture, localID)
		if _, err := conn.Exec(ctx, `INSERT INTO channel_messages (
 channel_id, local_id, from_id, message, file_id
) VALUES ($1, $2, $3, 'counter media fixture', $4)`, channelID, localID, ownerID, fileID); err != nil {
			t.Fatalf("insert %s search-counter channel post: %v", fixture.name, err)
		}
	}
}

func TestSearchCountersStatementCostAtScale(t *testing.T) {
	ctx := context.Background()
	tracer := &historyQueryCounter{}
	s, dsn := openStoreDSNWithOptions(t, store.WithQueryTracer(tracer))
	owner, err := s.CreateUser(ctx, "+15551248991")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.CreateUser(ctx, "+15551248992")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	_, err = conn.Exec(ctx, `INSERT INTO messages (owner_id, local_id, peer_id, peer_type, from_id, message, out)
 SELECT $1, n, $2, 1, $1, CASE WHEN n % 2 = 0 THEN 'https://example.test/a' ELSE 'plain' END, true FROM generate_series(1, 10000) n`, owner.ID, peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedSearchCounterDialogMedia(t, ctx, conn, s, owner.ID, peer.ID)
	filters := []tg.MessagesFilterClass{&tg.InputMessagesFilterDocument{}, &tg.InputMessagesFilterPhotos{}, &tg.InputMessagesFilterURL{}, &tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{}, &tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{}, &tg.InputMessagesFilterPhotoVideo{}}
	request := &tg.MessagesGetSearchCountersRequest{Peer: api.InputPeerUser(owner.ID, peer.ID), Filters: filters[:1]}
	tracer.reset()
	if _, err := api.SearchCountersForTest(s, owner.ID, request); err != nil {
		t.Fatal(err)
	}
	one := tracer.total()
	request.Filters = filters
	tracer.reset()
	got, err := api.SearchCountersForTest(s, owner.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	many := tracer.total()
	if many != one {
		t.Fatalf("statement count with %d filters = %d, one filter = %d", len(filters), many, one)
	}
	counters, ok := got.(*tg.MessagesSearchCounterVector)
	if !ok {
		t.Fatalf("counters = %T, want search counter vector", got)
	}
	if counters.Elems[2].Count != 5000 {
		t.Fatalf("URL count = %d, want 5000", counters.Elems[2].Count)
	}
	wantCounts := []int{6, 1, 5000, 1, 1, 1, 2, 1, 2}
	for i, filter := range []store.MediaSearchFilter{
		store.MediaSearchFilterDocument, store.MediaSearchFilterPhoto, store.MediaSearchFilterURL,
		store.MediaSearchFilterVideo, store.MediaSearchFilterGif, store.MediaSearchFilterPoll, store.MediaSearchFilterRoundVoice,
		store.MediaSearchFilterMusic, store.MediaSearchFilterPhotoVideo,
	} {
		_, want, err := s.SearchFilteredMessages(ctx, owner.ID, store.PeerTypeUser, peer.ID, "", filter, 0, 0, 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if counters.Elems[i].Count != want {
			t.Fatalf("filter %v count = %d, individual count = %d", filter, counters.Elems[i].Count, want)
		}
		if want == 0 {
			t.Fatalf("filter %v fixture count = 0, want a non-zero count", filter)
		}
		if want != wantCounts[i] {
			t.Fatalf("filter %v individual fixture count = %d, want %d", filter, want, wantCounts[i])
		}
	}
}

func TestChannelSearchCountersStatementCost(t *testing.T) {
	ctx := context.Background()
	tracer := &historyQueryCounter{}
	s, dsn := openStoreDSNWithOptions(t, store.WithQueryTracer(tracer))
	owner, err := s.CreateUser(ctx, "+15551248993")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "counter cost", "", true)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	_, err = conn.Exec(ctx, `INSERT INTO channel_messages (channel_id, local_id, from_id, message)
 SELECT $1, n, $2, CASE WHEN n % 2 = 0 THEN 'https://example.test/a' ELSE 'plain' END FROM generate_series(2, 101) n`, channel.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedSearchCounterChannelMedia(t, ctx, conn, owner.ID, channel.ID)
	filters := []tg.MessagesFilterClass{&tg.InputMessagesFilterDocument{}, &tg.InputMessagesFilterPhotos{}, &tg.InputMessagesFilterURL{}, &tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{}, &tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{}, &tg.InputMessagesFilterPhotoVideo{}}
	request := &tg.MessagesGetSearchCountersRequest{Peer: channelPeer(owner.ID, channel.ID), Filters: filters[:1]}
	tracer.reset()
	if _, err := api.SearchCountersForTest(s, owner.ID, request); err != nil {
		t.Fatal(err)
	}
	one := tracer.total()
	request.Filters = filters
	tracer.reset()
	got, err := api.SearchCountersForTest(s, owner.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if many := tracer.total(); many != one {
		t.Fatalf("statement count with %d filters = %d, one filter = %d", len(filters), many, one)
	}
	counters, ok := got.(*tg.MessagesSearchCounterVector)
	if !ok {
		t.Fatalf("counters = %T, want search counter vector", got)
	}
	if counters.Elems[2].Count != 50 {
		t.Fatalf("URL count = %d, want 50", counters.Elems[2].Count)
	}
	wantCounts := []int{6, 1, 50, 1, 1, 0, 2, 1, 2}
	for i, filter := range []store.MediaSearchFilter{store.MediaSearchFilterDocument, store.MediaSearchFilterPhoto, store.MediaSearchFilterURL, store.MediaSearchFilterVideo, store.MediaSearchFilterGif, store.MediaSearchFilterPoll, store.MediaSearchFilterRoundVoice, store.MediaSearchFilterMusic, store.MediaSearchFilterPhotoVideo} {
		_, want, err := s.SearchFilteredChannelPosts(ctx, owner.ID, channel.ID, "", filter, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if counters.Elems[i].Count != want {
			t.Fatalf("filter %v count = %d, individual count = %d", filter, counters.Elems[i].Count, want)
		}
		if filter != store.MediaSearchFilterPoll && want == 0 {
			t.Fatalf("filter %v fixture count = 0, want a non-zero count", filter)
		}
		if filter == store.MediaSearchFilterPoll && want != 0 {
			t.Fatalf("filter %v count = %d, want zero because channel polls are unsupported", filter, want)
		}
		if want != wantCounts[i] {
			t.Fatalf("filter %v individual fixture count = %d, want %d", filter, want, wantCounts[i])
		}
	}
}

// The URL count runs on the connection's serve loop, so the dot and "://"
// prefilter that keeps it off the regex for plain text must not drop a match.
func TestSearchCountersURLPrefilterKeepsEveryForm(t *testing.T) {
	ctx := context.Background()
	s, dsn := openStoreDSNWithOptions(t)
	owner, err := s.CreateUser(ctx, "+15551248994")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.CreateUser(ctx, "+15551248995")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "url forms", "", true)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	texts := []string{
		"http://localhost", "see www.example.com", "example.com/path", "HTTPS://EXAMPLE.IO", "tg://resolve",
		"no link here", "version 1.2", "end of sentence.", "",
	}
	const wantURLs = 5
	for i, text := range texts {
		n := int64(i + 2)
		if _, err := conn.Exec(ctx, `INSERT INTO messages (owner_id, local_id, peer_id, peer_type, from_id, message, out) VALUES ($1, $2, $3, 1, $1, $4, true)`, owner.ID, n, peer.ID, text); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO channel_messages (channel_id, local_id, from_id, message) VALUES ($1, $2, $3, $4)`, channel.ID, n, owner.ID, text); err != nil {
			t.Fatal(err)
		}
	}
	for name, peerClass := range map[string]tg.InputPeerClass{
		"dialog":  api.InputPeerUser(owner.ID, peer.ID),
		"channel": channelPeer(owner.ID, channel.ID),
	} {
		got, err := api.SearchCountersForTest(s, owner.ID, &tg.MessagesGetSearchCountersRequest{Peer: peerClass, Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterURL{}}})
		if err != nil {
			t.Fatal(err)
		}
		counters, ok := got.(*tg.MessagesSearchCounterVector)
		if !ok || len(counters.Elems) != 1 {
			t.Fatalf("%s counters = %T", name, got)
		}
		if counters.Elems[0].Count != wantURLs {
			t.Fatalf("%s URL count = %d, want %d", name, counters.Elems[0].Count, wantURLs)
		}
	}
}
