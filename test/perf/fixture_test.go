//go:build performance

package perf_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gotdsrp "github.com/gotd/td/crypto/srp"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	tsrp "github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

// fixtureVersion changes whenever the seeded shape changes, so a cached dump
// of an older shape is never measured as if it were this one.
const fixtureVersion = 8

// spec is one fixture size. The small fixture uses one hundredth of the large
// fixture's scalable data, subject to its Store's per-account contact cap.
type spec struct {
	Name          string `json:"name"`
	Contacts      int    `json:"contacts"`
	Dialogs       int    `json:"dialogs"`
	ChatMessages  int    `json:"chat_messages"`
	Subscribers   int    `json:"subscribers"`
	GroupMessages int    `json:"group_messages"`
	GroupPhotos   int    `json:"group_photos"`
	DayEvents     int    `json:"day_events"`
}

// The group's membership stays the same size in both fixtures: it is a fixed
// request shape, not account size.
const (
	groupMembers = 100
	needleEvery  = 10
)

// Difference measurements hold in-window event volume constant across fixture
// sizes so row reads measure database scale, not response volume.
const differenceEventsPerDay = 20

var fixtures = []spec{
	scaled("1x", 100),
	scaled("100x", shrink()),
}

// shrink lets a development run seed a smaller large fixture in minutes
// instead of the full one. CI never sets it; the fixture records its spec, so
// a shrunk fixture's numbers are never mistaken for the real one's.
func shrink() int {
	n, err := strconv.Atoi(os.Getenv("TG_PERF_SHRINK"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func scaled(name string, divisor int) spec {
	return spec{
		Name:     name,
		Contacts: min(contactCap, max(groupMembers, 10_000/divisor)),
		// Both fixtures need a full getDialogs page so the scale comparison
		// measures account size rather than a different response length.
		Dialogs:       max(pageLimit, 5_000/divisor),
		ChatMessages:  1_000_000 / divisor,
		Subscribers:   100_000 / divisor,
		GroupMessages: 200_000 / divisor,
		GroupPhotos:   max(2, 200/divisor),
		DayEvents:     differenceEventsPerDay,
	}
}

func TestFixtureContactsRespectPerformanceStoreLimit(t *testing.T) {
	small := scaled("1x", 100)
	large := scaled("100x", 1)
	if want := min(groupMembers, contactCap); small.Contacts != want {
		t.Fatalf("1x fixture contacts = %d, want %d", small.Contacts, want)
	}
	if want := min(10_000, contactCap); large.Contacts != want {
		t.Fatalf("100x fixture contacts = %d, want capped value %d", large.Contacts, want)
	}
}

func TestFixtureContactsStayWithinAccountBounds(t *testing.T) {
	for _, fixture := range fixtures {
		if fixture.Contacts < groupMembers {
			t.Errorf("%s contacts = %d, want at least %d for group membership", fixture.Name, fixture.Contacts, groupMembers)
		}
		if fixture.Contacts > contactCap {
			t.Errorf("%s contacts = %d, performance store limit = %d", fixture.Name, fixture.Contacts, contactCap)
		}
	}
}

func TestDialogsFixtureFillsGetDialogsPage(t *testing.T) {
	for _, fixture := range fixtures {
		if fixture.Dialogs < pageLimit {
			t.Errorf("%s fixture has %d dialogs, getDialogs page needs %d", fixture.Name, fixture.Dialogs, pageLimit)
		}
	}
}

func TestMessageSearchFixtureFillsOneXPage(t *testing.T) {
	small := scaled("1x", 100)
	if got := needleMessageCount(small.ChatMessages); got < pageLimit {
		t.Fatalf("1x fixture has %d needle messages, messages.search page needs %d", got, pageLimit)
	}
}

func needleMessageCount(messages int) int {
	if messages <= 0 {
		return 0
	}
	return (messages-1)/needleEvery + 1
}

func TestDifferenceFixtureEventsStayConstantAcrossScale(t *testing.T) {
	small := scaled("1x", 100)
	large := scaled("100x", 1)
	if small.DayEvents != differenceEventsPerDay || large.DayEvents != differenceEventsPerDay {
		t.Fatalf("difference day events = %d at 1x and %d at 100x, want %d at both scales", small.DayEvents, large.DayEvents, differenceEventsPerDay)
	}
}

// fixture is what the seeder leaves for the harness: the ids every scenario
// addresses. It is written next to the database dump and cached with it.
type fixture struct {
	Version      int     `json:"version"`
	Spec         spec    `json:"spec"`
	Owner        int64   `json:"owner"`
	Partner      int64   `json:"partner"`
	Subscriber   int64   `json:"subscriber"`
	Channel      int64   `json:"channel"`
	Group        int64   `json:"group"`
	Contacts     []int64 `json:"contacts"`
	OldestChatID int     `json:"oldest_chat_id"`
	MiddleDate   int     `json:"middle_date"`
}

func fixtureDSN(s spec) (string, bool) {
	dsn := os.Getenv("TG_PERF_DSN_" + envName(s))
	return dsn, dsn != ""
}

func envName(s spec) string {
	if s.Name == "1x" {
		return "1X"
	}
	return "100X"
}

func perfDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TG_PERF_DIR")
	if dir == "" {
		t.Skip("TG_PERF_DIR is not set; see test/perf/README.md")
	}
	return dir
}

func fixturePath(dir string, s spec) string { return filepath.Join(dir, "fixture-"+s.Name+".json") }
func blobDir(dir string, s spec) string     { return filepath.Join(dir, "blobs-"+s.Name) }

func loadFixture(path string) (fixture, error) {
	var f fixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.Version != fixtureVersion {
		return f, fmt.Errorf("%s is fixture version %d, want %d", path, f.Version, fixtureVersion)
	}
	return f, nil
}

func TestContactCapIncludesSeededContacts(t *testing.T) {
	if got, want := contactCap, 10_000; got < want {
		t.Fatalf("performance store contact cap = %d, want at least %d for seeded contacts", got, want)
	}
}

func TestLoadFixtureRejectsOlderVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	oldVersion := fixtureVersion - 1
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%d}`, oldVersion)), 0o600); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("fixture version %d, want %d", oldVersion, fixtureVersion)
	if _, err := loadFixture(path); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("loadFixture error = %v, want %s", err, want)
	}
}

// TestSeedFixtures writes both fixtures through the store's own write
// methods, the ones the RPC handlers call, and the photos through the RPC
// upload path. It is idempotent per fixture: a fixture whose description is
// already on disk is left alone.
func TestSeedFixtures(t *testing.T) {
	for _, s := range fixtures {
		if s.Contacts > contactCap {
			t.Fatalf("%s fixture has %d contacts, performance store limit is %d", s.Name, s.Contacts, contactCap)
		}
	}

	dir := perfDir(t)
	for _, s := range fixtures {
		t.Run(s.Name, func(t *testing.T) {
			dsn, ok := fixtureDSN(s)
			if !ok {
				t.Skipf("TG_PERF_DSN_%s is not set", envName(s))
			}
			if _, err := loadFixture(fixturePath(dir, s)); err == nil {
				t.Logf("%s already seeded", s.Name)
				return
			}
			started := time.Now()
			f := seed(t, t.Context(), dsn, blobDir(dir, s), s)
			raw, err := json.MarshalIndent(f, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixturePath(dir, s), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("seeded %s in %s", s.Name, time.Since(started).Round(time.Second))
		})
	}
}

func seed(t *testing.T, ctx context.Context, dsn, blobs string, s spec) fixture {
	t.Helper()
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// Seeding only writes; commit durability is irrelevant to what it leaves
	// behind and the session setting is not part of the dump.
	srv := bootServer(t, ctx, dsn+sessionOption(dsn, "synchronous_commit=off"), blobs, key)
	defer srv.stop()
	st := srv.store
	f := fixture{Version: fixtureVersion, Spec: s}

	owner := mustPerfOwner(t, ctx, st)
	partner := mustUser(t, ctx, st, "+19990000002", "Perf", "Partner")
	f.Owner, f.Partner = owner, partner
	f.Contacts = createUsers(t, ctx, st, "+1991", s.Contacts, true)
	subscribers := createUsers(t, ctx, st, "+1992", s.Subscribers, false)
	f.Subscriber = subscribers[0]
	channel, err := st.CreateChannel(ctx, owner, "Perf Channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group, err := st.CreateChannel(ctx, owner, "Perf Group", "", true)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	f.Channel, f.Group = channel.ID, group.ID
	members := f.Contacts[:groupMembers]
	addMembers(t, ctx, st, group.ID, owner, members)

	var progress atomic.Int64
	stopProgress := reportProgress(t, &progress)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		for i := range s.ChatMessages {
			from, to := owner, partner
			if i%2 == 1 {
				from, to = partner, owner
			}
			if _, _, _, _, err := st.SendMessage(gctx, from, to, chatText(i), int64(i+1), 0, 0); err != nil {
				return fmt.Errorf("chat message %d: %w", i, err)
			}
			progress.Add(1)
		}
		return nil
	})
	g.Go(func() error {
		for i := range s.GroupMessages {
			from := members[i%len(members)]
			if _, _, _, err := st.PostChannelMessageAs(gctx, group.ID, from, fmt.Sprintf("group post %d", i), int64(i+1), nil, 0); err != nil {
				return fmt.Errorf("group post %d: %w", i, err)
			}
			progress.Add(1)
		}
		return nil
	})
	g.Go(func() error {
		for _, c := range f.Contacts {
			if _, err := st.AddContact(gctx, owner, c); err != nil {
				return fmt.Errorf("add contact: %w", err)
			}
		}
		for i, c := range f.Contacts[:s.Dialogs] {
			if _, _, _, _, err := st.SendMessage(gctx, owner, c, "hello", int64(s.ChatMessages+i+1), 0, 0); err != nil {
				return fmt.Errorf("dialog %d: %w", i, err)
			}
		}
		return nil
	})
	g.Go(func() error {
		for i := 0; i < len(subscribers); i += 500 {
			batch := subscribers[i:min(i+500, len(subscribers))]
			added, err := st.AddChannelMembers(gctx, channel.ID, owner, batch)
			if err != nil {
				return fmt.Errorf("subscribers: %w", err)
			}
			if len(added) != len(batch) {
				return fmt.Errorf("subscribers: added %d of %d", len(added), len(batch))
			}
		}
		return nil
	})
	err = g.Wait()
	stopProgress()
	if err != nil {
		t.Fatal(err)
	}

	c := srv.login(t, ctx, owner)
	defer c.close()
	seedPhotos(t, ctx, c.api, owner, group.ID, s.GroupPhotos)
	f.OldestChatID, f.MiddleDate = chatBounds(t, ctx, c.api, owner, partner)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	if _, err := conn.Exec(ctx, "VACUUM ANALYZE"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	return f
}

// sessionOption appends a server setting for every connection the DSN opens.
func sessionOption(dsn, setting string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return sep + "options=" + strings.ReplaceAll(url.QueryEscape("-c "+setting), "+", "%20")
}

func chatText(i int) string {
	if i%needleEvery == 0 {
		return fmt.Sprintf("needle %d in the long chat", i)
	}
	return fmt.Sprintf("long chat message %d", i)
}

func phone(prefix string, i int) string { return fmt.Sprintf("%s%07d", prefix, i) }

func mustUser(t *testing.T, ctx context.Context, st *store.Store, phone, first, last string) int64 {
	t.Helper()
	u, err := st.CreateUser(ctx, phone)
	if err != nil {
		t.Fatalf("create user %s: %v", phone, err)
	}
	if err := st.UpdateProfile(ctx, u.ID, &first, &last); err != nil {
		t.Fatalf("name user %s: %v", phone, err)
	}
	return u.ID
}

func mustPerfOwner(t *testing.T, ctx context.Context, st *store.Store) int64 {
	t.Helper()
	salt1 := make([]byte, 32)
	if _, err := rand.Read(salt1); err != nil {
		t.Fatalf("generate owner password salt1: %v", err)
	}
	defer clear(salt1)
	salt2 := make([]byte, 32)
	if _, err := rand.Read(salt2); err != nil {
		t.Fatalf("generate owner password salt2: %v", err)
	}
	defer clear(salt2)

	password := []byte(fixtureOwnerPassword)
	defer clear(password)
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(rand.Reader).NewHash(password, gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     tsrp.G,
		P:     tsrp.PBytes(),
	})
	if err != nil {
		t.Fatalf("generate owner password verifier: %v", err)
	}
	defer clear(verifier)
	defer clear(augmentedSalt1)

	owner, err := st.CreateUsernameAccountWithPassword(ctx, fixtureOwnerUsername, augmentedSalt1, salt2, verifier)
	if err != nil {
		t.Fatalf("create owner username account: %v", err)
	}
	first, last := "Perf", "Owner"
	if err := st.UpdateProfile(ctx, owner.ID, &first, &last); err != nil {
		t.Fatalf("name owner: %v", err)
	}
	return owner.ID
}

// createUsers creates n accounts in parallel. Named ones carry a last name
// contacts.search can find: "k" and the zero-padded index.
func createUsers(t *testing.T, ctx context.Context, st *store.Store, prefix string, n int, named bool) []int64 {
	t.Helper()
	ids := make([]int64, n)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := range n {
		g.Go(func() error {
			u, err := st.CreateUser(gctx, phone(prefix, i))
			if err != nil {
				return err
			}
			ids[i] = u.ID
			if named {
				first, last := "Perf", fmt.Sprintf("k%05d", i)
				return st.UpdateProfile(gctx, u.ID, &first, &last)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("create users %s: %v", prefix, err)
	}
	return ids
}

func addMembers(t *testing.T, ctx context.Context, st *store.Store, channelID, owner int64, ids []int64) {
	t.Helper()
	added, err := st.AddChannelMembers(ctx, channelID, owner, ids)
	if err != nil || len(added) != len(ids) {
		t.Fatalf("add members: added %d of %d: %v", len(added), len(ids), err)
	}
}

func reportProgress(t *testing.T, n *atomic.Int64) func() {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				t.Logf("seeded %d messages", n.Load())
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

func seedPhotos(t *testing.T, ctx context.Context, c *tg.Client, owner, group int64, n int) {
	t.Helper()
	body := photoJPEG(t)
	peer := &tg.InputPeerChannel{ChannelID: group, AccessHash: pgtest.PeerDeriver().Derive(owner, peerhash.KindChannel, group)}
	for i := range n {
		fileID := int64(0x7065_7266_0000 + i)
		if _, err := c.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: fileID, FilePart: 0, Bytes: body}); err != nil {
			t.Fatalf("upload photo %d: %v", i, err)
		}
		if _, err := c.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     peer,
			Media:    &tg.InputMediaUploadedPhoto{File: &tg.InputFile{ID: fileID, Parts: 1, Name: "perf.jpg"}},
			Message:  fmt.Sprintf("group photo %d", i),
			RandomID: int64(1<<40 + i),
		}); err != nil {
			t.Fatalf("send photo %d: %v", i, err)
		}
	}
}

func photoJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := range 320 {
		for x := range 320 {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255}) // #nosec G115 -- wraps on purpose
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chatBounds finds the owner's oldest message id in the long chat and a date
// half way through it, by walking from the oldest end once.
func chatBounds(t *testing.T, ctx context.Context, c *tg.Client, owner, partner int64) (int, int) {
	t.Helper()
	peer := userPeer(owner, partner)
	newest, err := historyPage(ctx, c, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 1})
	if err != nil || len(newest) == 0 {
		t.Fatalf("newest chat message: %v", err)
	}
	oldest, err := historyPage(ctx, c, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: 1, AddOffset: -1, Limit: 1})
	if err != nil {
		t.Fatalf("oldest chat message: %v", err)
	}
	if len(oldest) == 0 {
		t.Fatal("oldest chat message: empty page")
	}
	return oldest[0].ID, (oldest[0].Date + newest[0].Date) / 2
}

func historyPage(ctx context.Context, c *tg.Client, req *tg.MessagesGetHistoryRequest) ([]*tg.Message, error) {
	res, err := c.MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, err
	}
	modified, ok := res.AsModified()
	if !ok {
		return nil, errors.New("history not modified")
	}
	var out []*tg.Message
	for _, m := range modified.GetMessages() {
		if msg, ok := m.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out, nil
}

func userPeer(viewer, target int64) *tg.InputPeerUser {
	return &tg.InputPeerUser{UserID: target, AccessHash: pgtest.PeerDeriver().Derive(viewer, peerhash.KindUser, target)}
}

func channelPeer(viewer, channel int64) *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: channel, AccessHash: pgtest.PeerDeriver().Derive(viewer, peerhash.KindChannel, channel)}
}

func inputChannel(viewer, channel int64) *tg.InputChannel {
	return &tg.InputChannel{ChannelID: channel, AccessHash: pgtest.PeerDeriver().Derive(viewer, peerhash.KindChannel, channel)}
}
