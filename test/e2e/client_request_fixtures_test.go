package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/tdp"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/config"
)

const clientFixtureDirectory = "../fixtures/client/layer-228"

func validClientFixtureMetadata(fixture clientRequestFixture) bool {
	return fixture.Layer == 228 &&
		(fixture.Client == "Teagram Desktop 7.0.9" || fixture.Client == "Teagram Desktop dev b70b98b" || fixture.Client == "gotd v0.162.0" || fixture.Client == "gotd/td v0.162.0")
}

func TestValidClientFixtureMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		client string
		want   bool
	}{
		{name: "desktop", client: "Teagram Desktop 7.0.9", want: true},
		{name: "desktop dev", client: "Teagram Desktop dev b70b98b", want: true},
		{name: "gotd legacy", client: "gotd v0.162.0", want: true},
		{name: "gotd td", client: "gotd/td v0.162.0", want: true},
		{name: "unsupported", client: "unknown", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := clientRequestFixture{Layer: 228, Client: test.client}
			if got := validClientFixtureMetadata(fixture); got != test.want {
				t.Errorf("validClientFixtureMetadata(%q) = %v, want %v", test.client, got, test.want)
			}
		})
	}
}

var errNilClientTLValue = errors.New("nil TL value")

type clientRequestFixture struct {
	Layer   int    `json:"layer"`
	Client  string `json:"client"`
	Source  string `json:"source"`
	Method  string `json:"method"`
	Request any    `json:"request"`
}

type capturedClientRequest struct {
	method string
	shape  any
	err    error
}

type clientRequestRecorder struct {
	mu      sync.Mutex
	records []capturedClientRequest
}

func (r *clientRequestRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func (r *clientRequestRecorder) countMethod(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, record := range r.records {
		if record.method == method {
			count++
		}
	}
	return count
}

func (r *clientRequestRecorder) add(request capturedClientRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, request)
}

func (r *clientRequestRecorder) matchFixtureSince(start int, fixtureName string) error {
	fixture, err := readClientRequestFixture(fixtureName)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if start < 0 || start > len(r.records) {
		return fmt.Errorf("invalid request recorder index %d (record count %d)", start, len(r.records))
	}
	for i := len(r.records) - 1; i >= start; i-- {
		got := r.records[i]
		if got.method != fixture.Method {
			continue
		}
		if got.err != nil {
			return fmt.Errorf("capture %s request: %w", fixture.Method, got.err)
		}
		if err := compareClientFixtureValue(fixture.Request, got.shape, "request"); err != nil {
			return fmt.Errorf("%s does not match %s: %w", fixture.Method, fixtureName, err)
		}
		return nil
	}
	return fmt.Errorf("no %s request captured after recorder index %d", fixture.Method, start)
}

func captureClientRequests(recorder *clientRequestRecorder) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			object, ok := input.(tdp.Object)
			if ok {
				shape, err := clientTLObjectShape(object)
				recorder.add(capturedClientRequest{method: object.TypeInfo().Name, shape: shape, err: err})
			}
			return next.Invoke(ctx, input, output)
		}
	})
}

func clientTLObjectShape(object tdp.Object) (map[string]any, error) {
	if object == nil {
		return nil, errors.New("nil TL request")
	}
	if setter, ok := object.(interface{ SetFlags() }); ok {
		setter.SetFlags()
	}
	value := reflect.ValueOf(object)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return nil, fmt.Errorf("TL request %T is not a non-nil pointer", object)
	}
	value = value.Elem()
	info := object.TypeInfo()
	if info.Null {
		return nil, fmt.Errorf("TL request %q is null", info.Name)
	}
	fields := make(map[string]any, len(info.Fields))
	shape := map[string]any{"type": info.Name, "fields": fields}
	if flags := value.FieldByName("Flags"); flags.IsValid() {
		shape["flags"] = flags.Uint()
	}
	for _, field := range info.Fields {
		if field.Null {
			continue
		}
		value := value.FieldByName(field.Name)
		if !value.IsValid() {
			return nil, fmt.Errorf("TL field %q missing from %T", field.SchemaName, object)
		}
		got, err := clientTLValue(field.SchemaName, value)
		if err != nil {
			return nil, fmt.Errorf("TL field %q: %w", field.SchemaName, err)
		}
		fields[field.SchemaName] = got
	}
	return shape, nil
}

func clientTLValue(fieldName string, value reflect.Value) (any, error) {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return nil, errNilClientTLValue
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return nil, errNilClientTLValue
	}
	if value.CanAddr() && value.Addr().CanInterface() {
		if object, ok := reflect.TypeAssert[tdp.Object](value.Addr()); ok {
			return clientTLObjectShape(object)
		}
	}
	if value.CanInterface() {
		if object, ok := reflect.TypeAssert[tdp.Object](value); ok {
			return clientTLObjectShape(object)
		}
	}
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if fieldName == "bytes" {
				return map[string]any{"redacted": true, "length": int64(value.Len())}, nil
			}
			if value.Len() == 0 {
				return "", nil
			}
			return "REDACTED", nil
		}
		result := make([]any, value.Len())
		for i := range value.Len() {
			item, err := clientTLValue(fieldName, value.Index(i))
			if err != nil {
				return nil, err
			}
			result[i] = item
		}
		return result, nil
	case reflect.Struct:
		return nil, fmt.Errorf("unsupported TL struct %s", value.Type())
	case reflect.Bool:
		return value.Bool(), nil
	case reflect.String:
		if redactClientString(fieldName) {
			return "REDACTED", nil
		}
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if redactClientNumber(fieldName, value.Int()) {
			return "<N>", nil
		}
		return value.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), nil
	default:
		if value.CanInterface() {
			return fmt.Sprint(value.Interface()), nil
		}
		return nil, fmt.Errorf("unsupported TL value kind %s", value.Kind())
	}
}

func redactClientNumber(fieldName string, value int64) bool {
	if value == 0 {
		return false
	}
	switch fieldName {
	case "id", "api_id", "file_id", "photo_id", "chat_id", "channel_id", "user_id", "document_id", "access_hash", "random_id":
		return true
	default:
		return false
	}
}

func redactClientString(fieldName string) bool {
	switch fieldName {
	case "api_hash", "phone_number", "phone_code_hash", "phone_code", "token", "md5_checksum":
		return true
	default:
		return false
	}
}

func TestClientUploadFixtureChecksRedactedByteLength(t *testing.T) {
	for _, part := range []struct {
		fixture string
		index   int
		length  int
	}{
		{"upload_saveFilePart.1", 0, 32768},
		{"upload_saveFilePart.8", 7, 10239},
	} {
		t.Run(part.fixture, func(t *testing.T) {
			fixture, err := readClientRequestFixture(part.fixture)
			if err != nil {
				t.Fatal(err)
			}
			for _, length := range []int{part.length, part.length - 1, part.length + 1, 0} {
				request := &tg.UploadSaveFilePartRequest{FileID: 42, FilePart: part.index, Bytes: make([]byte, length)}
				shape, err := clientTLObjectShape(request)
				if err != nil {
					t.Fatal(err)
				}
				err = compareClientFixtureValue(fixture.Request, shape, "request")
				if length == part.length {
					if err != nil {
						t.Errorf("captured byte length %d: %v", length, err)
					}
				} else if err == nil {
					t.Errorf("accepted %d upload bytes, want %d", length, part.length)
				}
			}
		})
	}
}

func TestClientTLValueRedactsFileIDs(t *testing.T) {
	got, err := clientTLValue("file_id", reflect.ValueOf(int64(42)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "<N>" {
		t.Fatalf("redacted file_id = %#v, want <N>", got)
	}
}

func TestClientTLValueRedactsCustomEmojiDocumentIDs(t *testing.T) {
	got, err := clientTLValue("document_id", reflect.ValueOf(int64(42)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "<N>" {
		t.Fatalf("redacted document_id = %#v, want <N>", got)
	}
}

func TestClientTLValueRedactsAPIIDs(t *testing.T) {
	got, err := clientTLValue("api_id", reflect.ValueOf(int64(2040)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "<N>" {
		t.Fatalf("redacted api_id = %#v, want <N>", got)
	}
}

func TestClientTLValueRedactsLoginSecrets(t *testing.T) {
	for _, field := range []string{"api_hash", "phone_number", "phone_code_hash", "phone_code", "token"} {
		t.Run(field, func(t *testing.T) {
			got, err := clientTLValue(field, reflect.ValueOf("sensitive"))
			if err != nil {
				t.Fatal(err)
			}
			if got != "REDACTED" {
				t.Fatalf("redacted %s = %#v, want REDACTED", field, got)
			}
		})
	}
}

func TestClientTLValueRedactsProfilePhotoIdentifiers(t *testing.T) {
	for _, field := range []string{"photo_id", "md5_checksum"} {
		t.Run(field, func(t *testing.T) {
			var value reflect.Value
			if field == "photo_id" {
				value = reflect.ValueOf(int64(42))
			} else {
				value = reflect.ValueOf("dynamic-content-checksum")
			}
			got, err := clientTLValue(field, value)
			if err != nil {
				t.Fatal(err)
			}
			want := any("<N>")
			if field == "md5_checksum" {
				want = "REDACTED"
			}
			if got != want {
				t.Fatalf("redacted %s = %#v, want %#v", field, got, want)
			}
		})
	}
}

func readClientRequestFixture(name string) (clientRequestFixture, error) {
	path := filepath.Join(clientFixtureDirectory, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return clientRequestFixture{}, fmt.Errorf("read client request fixture %q: %w", name, err)
	}
	var fixture clientRequestFixture
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		return clientRequestFixture{}, fmt.Errorf("decode client request fixture %q: %w", name, err)
	}
	if !validClientFixtureMetadata(fixture) ||
		(name == "messages_getCommonChats.1" && fixture.Client != "gotd v0.162.0") ||
		fixture.Source == "" || fixture.Method == "" || fixture.Request == nil {
		return clientRequestFixture{}, fmt.Errorf("invalid client request fixture metadata in %q", name)
	}
	if !strings.HasPrefix(fixture.Source, "captures/") && !strings.HasPrefix(fixture.Source, "source-derived/") {
		return clientRequestFixture{}, fmt.Errorf("client request fixture %q has invalid source path %q", name, fixture.Source)
	}
	if _, err := os.Stat(filepath.Join(clientFixtureDirectory, fixture.Source)); err != nil {
		return clientRequestFixture{}, fmt.Errorf("read source request for fixture %q: %w", name, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return clientRequestFixture{}, fmt.Errorf("client request fixture %q has trailing data", name)
	}
	return fixture, nil
}

func compareClientFixtureValue(want, got any, path string) error {
	if marker, ok := want.(string); ok && (marker == "<N>" || marker == "REDACTED") {
		if marker == "<N>" {
			if got == "<N>" {
				return nil
			}
			switch got.(type) {
			case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
				return nil
			default:
				return fmt.Errorf("%s = %T, want a redacted integer", path, got)
			}
		}
		if got == nil || got == "" {
			return fmt.Errorf("%s is empty, want a redacted value", path)
		}
		return nil
	}
	switch want := want.(type) {
	case map[string]any:
		got, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s = %T, want object", path, got)
		}
		if len(want) != len(got) {
			return fmt.Errorf("%s has %d fields, want %d", path, len(got), len(want))
		}
		for key, wantValue := range want {
			gotValue, ok := got[key]
			if !ok {
				return fmt.Errorf("%s.%s is missing", path, key)
			}
			if err := compareClientFixtureValue(wantValue, gotValue, path+"."+key); err != nil {
				return err
			}
		}
		return nil
	case []any:
		got, ok := got.([]any)
		if !ok || len(got) != len(want) {
			return fmt.Errorf("%s = %T, want array of length %d", path, got, len(want))
		}
		for i := range want {
			if err := compareClientFixtureValue(want[i], got[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case json.Number:
		gotNumber, ok := clientInt64(got)
		if !ok {
			return fmt.Errorf("%s = %T, want integer %s", path, got, want)
		}
		wantNumber, err := want.Int64()
		if err != nil || gotNumber != wantNumber {
			return fmt.Errorf("%s = %d, want %s", path, gotNumber, want)
		}
		return nil
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Errorf("%s = %#v, want %#v", path, got, want)
		}
		return nil
	}
}

func clientInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case uint64:
		if value > 1<<63-1 {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

func TestSmokeClientRequestFixturesAreLayer228Shapes(t *testing.T) {
	for _, name := range []string{
		"auth.sendCode.1",
		"auth.signIn.1",
		"help_getAppConfig.1",
		"account_updatePasswordSettings.contact.1",
		"account_getCollectibleEmojiStatuses.1",
		"auth.invokeWithReCaptcha.signUp.1",
		"contacts_getStatuses.1",
		"channels_checkUsername.1",
		"channels_createChannel.1",
		"channels_editTitle.1",
		"channels_editPhoto.1",
		"channels_editPhoto.2",
		"channels_toggleSignatures.1",
		"channels_togglePreHistoryHidden.1",
		"channels_getFullChannel.1",
		"channels_getFullChannel.2",
		"channels_getFullChannel.3",
		"channels_getFullChannel.4",
		"channels_getFullChannel.5",
		"channels_inviteToChannel.1",
		"messages_getPinnedSavedDialogs.1",
		"messages_getSavedDialogs.1",
		"messages_getWebPage.1",
		"messages_getDialogs.1",
		"messages_getDialogs.2",
		"messages_getDialogs.3",
		"messages_createChat.1",
		"messages_deleteChat.1",
		"messages_editChatPhoto.1",
		"messages_editChatPhoto.2",
		"messages_getCommonChats.1",
		"users_getFullUser.bot.1",
		"messages_deleteHistory.1",
		"messages_editChatAbout.1",
		"messages_editChatAbout.2",
		"messages_editMessage.1",
		"messages_exportChatInvite.1",
		"messages_getFullChat.1",
		"messages_getHistory.1",
		"messages_getHistory.2",
		"messages_getHistory.second-account.1",
		"messages_getReplies.1",
		"photos_getUserPhotos.1",
		"photos_deletePhotos.1",
		"photos_updateProfilePhoto.1",
		"photos_uploadProfilePhoto.1",
		"account_updateProfile.1",
		"account_checkUsername.1",
		"messages_getAvailableReactions.1",
		"upload_getFile.1",
		"upload_getFile.2",
		"messages_sendMedia.1",
		"messages_sendMedia.2",
		"messages_sendMedia.3",
		"messages_sendMedia.4",
		"messages_sendMedia.sticker.1",
		"messages_sendMedia.gif.1",
		"stickers_createStickerSet.1",
		"messages_getCustomEmojiDocuments.1",
		"messages_sendMessage.customEmoji.private.1",
		"messages_sendMessage.customEmoji.group.1",
		"messages_sendMessage.customEmoji.channel.1",
		"messages_saveGif.1",
		"messages_saveGif.2",
		"messages_getSavedGifs.1",
		"messages_sendMessage.1",
		"messages_sendMessage.2",
		"messages_sendMessage.3",
		"messages_sendReaction.1",
		"messages_sendReaction.channel.1",
		"messages_setChatAvailableReactions.1",
		"messages_setChatAvailableReactions.channel.1",
		"messages_getMessagesReactions.1",
		"messages_getMessagesReactions.channel.1",
		"messages_getMessageReactionsList.1",
		"messages_sendVote.1",
		"messages_setTyping.1",
		"upload_saveFilePart.1",
		"upload_saveFilePart.8",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readClientRequestFixture(name)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSmokeClientRequestFixtures(t *testing.T) {
	testSmokeClientRequestFixtures(t)
}

func newSmokePhoneClientWithRequests(
	t *testing.T, f *smokeFixture, label, phone string, requests *clientRequestRecorder,
) *smokeClient {
	t.Helper()
	seedPhoneUsers(t, f.ctx, f.store, phone)
	user, ok, err := f.store.UserByPhone(f.ctx, phone)
	if err != nil || !ok {
		t.Fatalf("load phone account for %s: ok=%t err=%v", label, ok, err)
	}

	sess := &session.StorageMemory{}
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	clientCtx, cancelClient := context.WithCancel(f.ctx)
	client := &smokeClient{
		client:   f.managedClientWithDevice(sess, seen, push, manager, telegram.DeviceConfig{}, requests),
		requests: requests,
		cancel:   cancelClient,
		session:  sess,
		manager:  manager,
		seen:     seen,
		push:     push,
		cmds:     make(chan command),
		label:    label,
	}
	ids := make(chan int64, 1)
	client.lifecycle = startClientLifecycle(clientCtx, label, f.failures, func(*clientPhaseState) error {
		return runBoundInteractive(clientCtx, client.client, sess, f.store, user.ID, ids, client.cmds)
	})
	t.Cleanup(func() { client.stopClient(t) })
	select {
	case client.id = <-ids:
	case <-f.ctx.Done():
		t.Fatalf("%s login timeout", label)
	case <-client.lifecycle.result.done:
		t.Fatalf("%s login failed: %v", label, client.lifecycle.result.error())
	}
	return client
}

const clientRequestFixturesTimeout = 3 * time.Minute

func testSmokeClientRequestFixtures(t *testing.T) {
	t.Helper()
	// This replay covers many RPCs; allow room for the parallel CI smoke load.
	f := newSmokeFixtureWithDeadline(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.recoveryEmailMode = config.RecoveryEmailContact
	}, clientRequestFixturesTimeout)
	const phoneCreator, phoneMember = "+15551048901", "+15551048902"
	seedSmokeUsers(t, f, phoneCreator, phoneMember)
	creatorRequests := &clientRequestRecorder{}
	memberRequests := &clientRequestRecorder{}
	checkerRequests := &clientRequestRecorder{}
	creator := newSmokeClientWithRequests(t, f, "Client fixture creator", phoneCreator, creatorRequests)
	member := newSmokeClientWithRequests(t, f, "Client fixture member", phoneMember, memberRequests)
	checker := newSmokePhoneClientWithRequests(t, f, "Client fixture username checker", "+15551048903", checkerRequests)
	for _, fixtureName := range []string{"auth.sendCode.1", "auth.signIn.1"} {
		if err := creator.requests.matchFixtureSince(0, fixtureName); err != nil {
			t.Fatalf("replay login fixture %s: %v", fixtureName, err)
		}
	}
	if err := fixtureClientCall(t, creator, "help_getAppConfig.1", func(ctx context.Context, api *tg.Client) error {
		appConfig, err := api.HelpGetAppConfig(ctx, 0)
		if err != nil {
			return err
		}
		mode, err := stickerImportModeFromAppConfig(appConfig)
		if err != nil {
			return err
		}
		if mode != string(config.StickerImportOn) {
			return fmt.Errorf("sticker_import_mode = %q, want %q", mode, config.StickerImportOn)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay app-config sticker import preflight fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getAllDrafts.1", func(ctx context.Context, api *tg.Client) error {
		response, err := api.MessagesGetAllDrafts(ctx)
		if err != nil {
			return err
		}
		if _, ok := response.(*tg.Updates); !ok {
			return fmt.Errorf("getAllDrafts response = %T, want *tg.Updates", response)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-all-drafts fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "account_getCollectibleEmojiStatuses.1", func(ctx context.Context, api *tg.Client) error {
		return checkCollectibleEmojiStatuses(ctx, api, 0)
	}); err != nil {
		t.Fatalf("replay collectible emoji statuses fixture: %v", err)
	}
	const stickerSetShortName = "twemoji_reactions"
	stickerSetRef := &tg.InputStickerSetShortName{ShortName: stickerSetShortName}
	var stickerSet tg.StickerSet
	var stickerDocument *tg.Document
	if err := fixtureClientCall(t, creator, "messages_getAllStickers.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesAllStickers); !ok {
			return fmt.Errorf("getAllStickers result = %T, want *tg.MessagesAllStickers", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-all-stickers fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getStickerSet.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: stickerSetRef})
		if err != nil {
			return err
		}
		full, ok := result.(*tg.MessagesStickerSet)
		if !ok {
			return fmt.Errorf("getStickerSet result = %T, want *tg.MessagesStickerSet", result)
		}
		stickerSet = full.Set
		if len(full.Documents) == 0 {
			return errors.New("getStickerSet returned no sticker documents")
		}
		stickerDocument, ok = full.Documents[0].(*tg.Document)
		if !ok {
			return fmt.Errorf("first sticker document = %T, want *tg.Document", full.Documents[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-sticker-set fixture: %v", err)
	}
	stickerSetIDRef := &tg.InputStickerSetID{ID: stickerSet.ID, AccessHash: stickerSet.AccessHash}
	if err := fixtureClientCall(t, creator, "messages_getStickerSet.2", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: stickerSetIDRef})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSet); !ok {
			return fmt.Errorf("getStickerSet by id result = %T, want *tg.MessagesStickerSet", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-sticker-set-by-id fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_installStickerSet.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: stickerSetRef})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("installStickerSet result = %T, want success", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay install-sticker-set fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_installStickerSet.2", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: stickerSetIDRef, Archived: true})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("repeated installStickerSet result = %T, want success", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay install-sticker-set-by-id fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getArchivedStickers.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetArchivedStickers(ctx, &tg.MessagesGetArchivedStickersRequest{
			Masks: true, Emojis: true, OffsetID: 987654321, Limit: 25,
		})
		if err != nil {
			return err
		}
		if result.Count != 0 || len(result.Sets) != 0 {
			return fmt.Errorf("getArchivedStickers = count %d, %d sets after install; want empty", result.Count, len(result.Sets))
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-archived-stickers fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_uninstallStickerSet.1", func(ctx context.Context, api *tg.Client) error {
		removed, err := api.MessagesUninstallStickerSet(ctx, stickerSetRef)
		if err != nil {
			return err
		}
		if !removed {
			return errors.New("uninstallStickerSet returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay uninstall-sticker-set fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_uninstallStickerSet.2", func(ctx context.Context, api *tg.Client) error {
		removed, err := api.MessagesUninstallStickerSet(ctx, stickerSetIDRef)
		if err != nil {
			return err
		}
		if !removed {
			return errors.New("uninstallStickerSet by id returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay uninstall-sticker-set-by-id fixture: %v", err)
	}
	const recoveryPassword, recoveryEmail = "fixture recovery password", "contact@example.invalid"
	if err := fixtureClientCall(t, checker, "account_updatePasswordSettings.contact.1", func(ctx context.Context, api *tg.Client) error {
		return setFirstPasswordWithEmail(ctx, api, recoveryPassword, recoveryEmail)
	}); err != nil {
		t.Fatalf("set contact password: %v", err)
	}
	storedPassword, hasPassword, err := f.store.PasswordByUser(f.ctx, checker.id)
	if err != nil || !hasPassword {
		t.Fatalf("load contact password: has_password=%t err=%v", hasPassword, err)
	}
	if storedPassword.RecoveryEmail != recoveryEmail || storedPassword.HasRecovery {
		t.Fatalf("contact password state = {email:%q has_recovery:%t}, want contact email and false recovery flag", storedPassword.RecoveryEmail, storedPassword.HasRecovery)
	}
	if err := checker.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		state, err := api.AccountGetPassword(ctx)
		if err != nil {
			return err
		}
		if state.HasRecovery {
			return errors.New("account.getPassword advertised recovery in contact mode")
		}
		settings, err := getPasswordSettingsWithProof(ctx, api, recoveryPassword)
		if err != nil {
			return err
		}
		if email, present := settings.GetEmail(); !present || email != recoveryEmail {
			return fmt.Errorf("proved account.getPasswordSettings email = %q (present=%t), want %q", email, present, recoveryEmail)
		}
		return nil
	}); err != nil {
		t.Fatalf("proved contact settings smoke: %v", err)
	}
	for _, tc := range []struct {
		username string
		want     bool
	}{
		{username: "clientfixturefree", want: true},
		{username: smokeUsernameForPhone(phoneMember), want: false},
	} {
		var available bool
		if err := fixtureClientCall(t, checker, "account_checkUsername.1", func(ctx context.Context, api *tg.Client) error {
			var err error
			available, err = api.AccountCheckUsername(ctx, tc.username)
			return err
		}); err != nil {
			t.Fatalf("check username %q: %v", tc.username, err)
		}
		if available != tc.want {
			t.Errorf("account.checkUsername(%q) = %t, want %t", tc.username, available, tc.want)
		}
	}
	if err := fixtureClientCall(t, checker, "account_checkUsername.1", func(ctx context.Context, api *tg.Client) error {
		_, err := api.AccountCheckUsername(ctx, "bad-name")
		return err
	}); !tgerr.Is(err, "USERNAME_INVALID") {
		t.Fatalf("invalid account.checkUsername error = %v, want USERNAME_INVALID", err)
	}
	if err := fixtureClientCall(t, creator, "messages_hidePeerSettingsBar.1", func(ctx context.Context, api *tg.Client) error {
		hidden, err := api.MessagesHidePeerSettingsBar(ctx, peerUser(creator.id, member.id))
		if err != nil {
			return err
		}
		if !hidden {
			return errors.New("hide peer settings bar returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay hide-peer-settings-bar fixture: %v", err)
	}

	if err := fixtureClientCall(t, creator, "contacts_getStatuses.1", func(ctx context.Context, api *tg.Client) error {
		statuses, err := api.ContactsGetStatuses(ctx)
		if err != nil {
			return err
		}
		if len(statuses) != 0 {
			return fmt.Errorf("fixture creator contact statuses = %d, want empty", len(statuses))
		}
		return nil
	}); err != nil {
		t.Fatalf("replay contacts.getStatuses fixture: %v", err)
	}

	var reactionDocument *tg.Document
	if err := fixtureClientCall(t, creator, "messages_getAvailableReactions.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetAvailableReactions(ctx, 0)
		if err != nil {
			return err
		}
		available, ok := result.(*tg.MessagesAvailableReactions)
		if !ok || len(available.Reactions) == 0 {
			return fmt.Errorf("available reactions = %T, want non-empty *tg.MessagesAvailableReactions", result)
		}
		staticIcon, ok := available.Reactions[0].StaticIcon.(*tg.Document)
		if !ok {
			return fmt.Errorf("first available reaction static_icon = %T, want *tg.Document", available.Reactions[0].StaticIcon)
		}
		reactionDocument = staticIcon
		return nil
	}); err != nil {
		t.Fatalf("replay available-reactions fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "upload_getFile.2", func(ctx context.Context, api *tg.Client) error {
		if reactionDocument == nil {
			return errors.New("available reaction did not provide a document")
		}
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID: reactionDocument.ID, AccessHash: reactionDocument.AccessHash, FileReference: reactionDocument.FileReference,
			},
			Offset: 0,
			Limit:  262144,
		})
		if err != nil {
			return err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || len(file.Bytes) == 0 {
			return fmt.Errorf("reaction upload.getFile = %T, want non-empty *tg.UploadFile", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay reaction upload.getFile fixture: %v", err)
	}

	const savedNote = "client fixture Saved Messages note"
	var savedNoteResult tg.UpdatesClass
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		savedNoteResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: savedNote, RandomID: 13030112,
		})
		return err
	}); err != nil {
		t.Fatalf("send client fixture Saved Messages note: %v", err)
	}
	savedNoteMessage := assertSmokeSendResult(t, savedNoteResult, savedNote, 1, 1)
	var savedDialogs tg.MessagesSavedDialogsClass
	if err := fixtureClientCall(t, creator, "messages_getSavedDialogs.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		savedDialogs, err = api.MessagesGetSavedDialogs(ctx, &tg.MessagesGetSavedDialogsRequest{
			ExcludePinned: true,
			OffsetPeer:    &tg.InputPeerEmpty{},
			Limit:         20,
		})
		if err != nil {
			return err
		}
		response, ok := savedDialogs.(*tg.MessagesSavedDialogs)
		if !ok {
			return fmt.Errorf("saved dialogs response = %T, want *tg.MessagesSavedDialogs", savedDialogs)
		}
		if len(response.Dialogs) != 1 {
			return fmt.Errorf("saved dialogs returned %d dialogs, want My Notes", len(response.Dialogs))
		}
		dialog, ok := response.Dialogs[0].(*tg.SavedDialog)
		if !ok {
			return fmt.Errorf("saved dialog = %T, want *tg.SavedDialog", response.Dialogs[0])
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != creator.id || dialog.TopMessage != savedNoteMessage.ID || dialog.GetPinned() {
			return fmt.Errorf("saved dialog = %+v, want unpinned My Notes at message %d", dialog, savedNoteMessage.ID)
		}
		if len(response.Messages) != 1 {
			return fmt.Errorf("saved dialogs returned %d messages, want latest My Notes message", len(response.Messages))
		}
		message, ok := response.Messages[0].(*tg.Message)
		if !ok || message.ID != savedNoteMessage.ID || message.Message != savedNote || !message.Out {
			return fmt.Errorf("saved dialog message = %#v, want outgoing My Notes text %q at ID %d", response.Messages[0], savedNote, savedNoteMessage.ID)
		}
		if len(response.Chats) != 0 || len(response.Users) != 1 {
			return fmt.Errorf("saved dialogs entities = %d chats, %d users; want self user only", len(response.Chats), len(response.Users))
		}
		self, ok := response.Users[0].(*tg.User)
		if !ok || !self.Self || self.ID != creator.id {
			return fmt.Errorf("saved dialog user = %#v, want self user %d", response.Users[0], creator.id)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay saved-dialog fixture: %v", err)
	}

	var pinnedSavedDialogs tg.MessagesSavedDialogsClass
	if err := fixtureClientCall(t, creator, "messages_getPinnedSavedDialogs.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		pinnedSavedDialogs, err = api.MessagesGetPinnedSavedDialogs(ctx)
		if err != nil {
			return err
		}
		response, ok := pinnedSavedDialogs.(*tg.MessagesSavedDialogs)
		if !ok {
			return fmt.Errorf("pinned saved dialogs response = %T, want *tg.MessagesSavedDialogs", pinnedSavedDialogs)
		}
		if len(response.Dialogs) != 0 || len(response.Messages) != 0 || len(response.Chats) != 0 || len(response.Users) != 0 {
			return fmt.Errorf("pinned saved dialogs = %+v, want empty vectors", response)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay pinned saved-dialog fixture: %v", err)
	}

	if err := fixtureClientCall(t, creator, "messages_getWebPage.1", func(ctx context.Context, api *tg.Client) error {
		response, err := api.MessagesGetWebPage(ctx, &tg.MessagesGetWebPageRequest{
			URL:  "https://example.test/client-fixture",
			Hash: 0,
		})
		if err != nil {
			return err
		}
		webpage, ok := response.Webpage.(*tg.WebPageEmpty)
		if !ok || webpage.ID != 0 {
			return fmt.Errorf("webpage = %#v, want empty page without cached content", response.Webpage)
		}
		if url, hasURL := webpage.GetURL(); hasURL || url != "" {
			return fmt.Errorf("empty webpage URL = %q present=%v, want no cached URL", url, hasURL)
		}
		if len(response.Chats) != 0 || len(response.Users) != 0 {
			return fmt.Errorf("webpage entities = %d chats, %d users; want empty", len(response.Chats), len(response.Users))
		}
		return nil
	}); err != nil {
		t.Fatalf("replay web-page fixture: %v", err)
	}

	var chatID int64
	if err := fixtureClientCall(t, creator, "messages_createChat.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesCreateChatRequest{
			Title: "Client fixture group",
			Users: []tg.InputUserClass{inputUser(creator.id, member.id)},
		}
		request.SetTTLPeriod(0)
		created, err := api.MessagesCreateChat(ctx, request)
		if err != nil {
			return err
		}
		if len(created.MissingInvitees) != 0 {
			return fmt.Errorf("create fixture group missing %d invitees", len(created.MissingInvitees))
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok {
			return fmt.Errorf("fixture group updates = %T, want *tg.Updates", created.Updates)
		}
		if len(updates.Chats) != 1 {
			return fmt.Errorf("create fixture group returned %d chats, want 1", len(updates.Chats))
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("fixture group = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create fixture group: %v", err)
	}
	groupPhotoBody := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const groupPhotoFileID = int64(13030121)
	var groupPhotoResult tg.UpdatesClass
	if err := fixtureClientCall(t, creator, "messages_editChatPhoto.1", func(ctx context.Context, api *tg.Client) error {
		for part := range smokePhotoPartCount {
			start := part * smokePhotoPartSize
			ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: groupPhotoFileID, FilePart: part, Bytes: groupPhotoBody[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("group photo fixture upload part %d returned false", part)
			}
		}
		photo := &tg.InputChatUploadedPhoto{}
		photo.SetFile(&tg.InputFile{
			ID: groupPhotoFileID, Parts: smokePhotoPartCount, Name: "group.jpg", MD5Checksum: smokePhotoMD5(groupPhotoBody),
		})
		var err error
		groupPhotoResult, err = api.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{ChatID: chatID, Photo: photo})
		return err
	}); err != nil {
		t.Fatalf("replay edit-chat-photo upload fixture: %v", err)
	}
	photoFromEditUpdates(t, groupPhotoResult)
	var groupPhotoClear tg.UpdatesClass
	if err := fixtureClientCall(t, creator, "messages_editChatPhoto.2", func(ctx context.Context, api *tg.Client) error {
		var err error
		groupPhotoClear, err = api.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{
			ChatID: chatID, Photo: &tg.InputChatPhotoEmpty{},
		})
		return err
	}); err != nil {
		t.Fatalf("replay edit-chat-photo clear fixture: %v", err)
	}
	assertDeletePhotoAction(t, groupPhotoClear, "fixture clear")
	if err := fixtureClientCall(t, creator, "messages_getCommonChats.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetCommonChats(ctx, &tg.MessagesGetCommonChatsRequest{
			UserID: inputUser(creator.id, member.id),
			Limit:  10,
		})
		if err != nil {
			return err
		}
		page, ok := result.(*tg.MessagesChatsSlice)
		if !ok || page.Count != 1 || len(page.Chats) != 1 {
			return fmt.Errorf("messages.getCommonChats result = %#v, want one common group", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay common-chats fixture: %v", err)
	}

	for i, fixtureName := range []string{"messages_getDialogs.1", "messages_getDialogs.2", "messages_getDialogs.3"} {
		if err := fixtureClientCall(t, creator, fixtureName, func(ctx context.Context, api *tg.Client) error {
			request := &tg.MessagesGetDialogsRequest{
				ExcludePinned: true,
				OffsetPeer:    &tg.InputPeerEmpty{},
				Limit:         20,
			}
			request.SetFolderID(0)
			_, err := api.MessagesGetDialogs(ctx, request)
			return err
		}); err != nil {
			t.Fatalf("replay dialogs fixture %d: %v", i+1, err)
		}
	}
	if err := fixtureClientCall(t, creator, "messages_getFullChat.1", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesGetFullChat(ctx, chatID)
		return err
	}); err != nil {
		t.Fatalf("replay full-chat fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_editChatAbout.1", func(ctx context.Context, api *tg.Client) error {
		updated, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: "Fixture details",
		})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("editChatAbout returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay edit-chat-about fixture: %v", err)
	}

	olderProfilePhoto := []byte("older profile photo")
	newerProfilePhoto := []byte("newer profile photo")
	olderProfilePreview := []byte("older profile preview")
	newerProfilePreview := []byte("newer profile preview")
	profilePhotoIDs := []int64{
		seedSmokeProfilePhoto(t, f, member.id, olderProfilePhoto, olderProfilePreview),
		seedSmokeProfilePhoto(t, f, member.id, newerProfilePhoto, newerProfilePreview),
	}
	var profilePhotos tg.PhotosPhotosClass
	if err := fixtureClientCall(t, creator, "photos_getUserPhotos.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		profilePhotos, err = api.PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
			UserID: inputUser(creator.id, member.id),
			Offset: 0,
			MaxID:  0,
			Limit:  100,
		})
		return err
	}); err != nil {
		t.Fatalf("replay profile-photo fixture: %v", err)
	}
	profilePage, ok := profilePhotos.(*tg.PhotosPhotosSlice)
	if !ok || profilePage.Count != 2 || len(profilePage.Photos) != 2 {
		t.Fatalf("profile photos = %#v, want photosSlice count 2", profilePhotos)
	}
	for i, wantID := range []int64{profilePhotoIDs[1], profilePhotoIDs[0]} {
		photo, ok := profilePage.Photos[i].(*tg.Photo)
		if !ok || photo.ID != wantID {
			t.Fatalf("profile photo %d = %T/%+v, want newest-first id %d", i, profilePage.Photos[i], profilePage.Photos[i], wantID)
		}
		wantBytes := newerProfilePhoto
		wantPreview := newerProfilePreview
		if i == 1 {
			wantBytes = olderProfilePhoto
			wantPreview = olderProfilePreview
		}
		assertSmokeProfilePhotoSizes(t, creator.lifecycle.ctx, creator, photo, wantBytes, wantPreview, fmt.Sprintf("profile photo %d", i))
	}

	const profileAbout = "A fictional astronomer charting quiet northern skies."
	if err := fixtureClientCall(t, member, "account_updateProfile.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.AccountUpdateProfileRequest{}
		request.SetAbout(profileAbout)
		if _, err := api.AccountUpdateProfile(ctx, request); err != nil {
			return err
		}
		profile, err := api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
		if err != nil {
			return err
		}
		about, ok := profile.FullUser.GetAbout()
		if !ok || about != profileAbout {
			return fmt.Errorf("profile about = %q/%t, want %q", about, ok, profileAbout)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay account.updateProfile fixture: %v", err)
	}

	profilePhotoPayload := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const profilePhotoFileID = int64(13030113)
	profilePhotoParts := uploadSmokeMediaParts(t, f.ctx, member, profilePhotoFileID, profilePhotoPayload, smokePhotoPartSize)
	var uploadedProfilePhotoID int64
	if err := fixtureClientCall(t, member, "photos_uploadProfilePhoto.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.PhotosUploadProfilePhotoRequest{}
		request.SetFile(&tg.InputFile{
			ID: profilePhotoFileID, Parts: profilePhotoParts, Name: "fictional-avatar.jpg", MD5Checksum: smokePhotoMD5(profilePhotoPayload),
		})
		result, err := api.PhotosUploadProfilePhoto(ctx, request)
		if err != nil {
			return err
		}
		photo, ok := result.Photo.(*tg.Photo)
		if !ok || photo.ID <= 0 {
			return fmt.Errorf("uploaded profile photo = %T/%+v, want photo with an id", result.Photo, result.Photo)
		}
		uploadedProfilePhotoID = photo.ID
		return nil
	}); err != nil {
		t.Fatalf("replay photos.uploadProfilePhoto fixture: %v", err)
	}

	if err := fixtureClientCall(t, creator, "upload_getFile.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPeerPhotoFileLocation{
				Peer: peerUser(creator.id, member.id), PhotoID: uploadedProfilePhotoID, Big: true,
			},
			Offset: 0,
			Limit:  len(profilePhotoPayload),
		})
		if err != nil {
			return err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || !bytes.Equal(file.Bytes, profilePhotoPayload) {
			return fmt.Errorf("current profile photo download = %T, want original JPEG bytes", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay upload.getFile peer-photo fixture: %v", err)
	}

	var channelID int64
	if err := fixtureClientCall(t, creator, "channels_createChannel.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.ChannelsCreateChannelRequest{Title: "Channel 1", About: "Description of channel 1"}
		request.SetBroadcast(true)
		result, err := api.ChannelsCreateChannel(ctx, request)
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok {
			return fmt.Errorf("create fixture channel result = %T, want *tg.Updates", result)
		}
		for _, chat := range updates.Chats {
			if channel, ok := chat.(*tg.Channel); ok {
				channelID = channel.ID
				return nil
			}
		}
		return errors.New("create fixture channel returned no channel")
	}); err != nil {
		t.Fatalf("create fixture channel: %v", err)
	}
	channelPhotoBody := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const channelPhotoFileID = int64(13030122)
	var channelPhotoID int64
	if err := fixtureClientCall(t, creator, "channels_editPhoto.1", func(ctx context.Context, api *tg.Client) error {
		for part := range smokePhotoPartCount {
			start := part * smokePhotoPartSize
			ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: channelPhotoFileID, FilePart: part, Bytes: channelPhotoBody[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("channel photo fixture upload part %d returned false", part)
			}
		}
		photo := &tg.InputChatUploadedPhoto{}
		photo.SetFile(&tg.InputFile{
			ID: channelPhotoFileID, Parts: smokePhotoPartCount, Name: "channel.jpg", MD5Checksum: smokePhotoMD5(channelPhotoBody),
		})
		if _, err := api.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
			Channel: inputChannel(creator.id, channelID), Photo: photo,
		}); err != nil {
			return err
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		fullPhoto, ok := full.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("channels.getFullChannel full chat = %T, want *tg.ChannelFull", full.FullChat)
		}
		currentPhoto, ok := fullPhoto.ChatPhoto.(*tg.Photo)
		if !ok || currentPhoto.ID <= 0 {
			return fmt.Errorf("channels.getFullChannel photo = %#v, want non-empty *tg.Photo", fullPhoto.ChatPhoto)
		}
		channelPhotoID = currentPhoto.ID
		if !channelFullPhoto(full, channelID, channelPhotoID) {
			return errors.New("channels.editPhoto photo was not returned by channels.getFullChannel")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel edit-photo upload fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "channels_editPhoto.2", func(ctx context.Context, api *tg.Client) error {
		if _, err := api.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
			Channel: inputChannel(creator.id, channelID), Photo: &tg.InputChatPhotoEmpty{},
		}); err != nil {
			return err
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		if !channelFullPhotoEmpty(full, channelID) {
			return errors.New("channels.editPhoto clear left a current channel photo")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel edit-photo clear fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "channels_editTitle.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.ChannelsEditTitle(ctx, &tg.ChannelsEditTitleRequest{
			Channel: inputChannel(creator.id, channelID),
			Title:   "Channel 1 renamed",
		})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.Updates); !ok {
			return fmt.Errorf("edit fixture channel title result = %T, want *tg.Updates", result)
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		if !channelTitle(full.Chats, channelID, "Channel 1 renamed") {
			return errors.New("edit fixture channel title was not stored")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel title fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "channels_toggleSignatures.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.ChannelsToggleSignaturesRequest{Channel: inputChannel(creator.id, channelID)}
		request.SetSignaturesEnabled(true)
		result, err := api.ChannelsToggleSignatures(ctx, request)
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok {
			return fmt.Errorf("toggle fixture channel signatures result = %T, want *tg.Updates", result)
		}
		if !channelSignaturesEnabled(updates.Chats, channelID) {
			return errors.New("toggle fixture channel signatures did not return the enabled channel")
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		if !channelSignaturesEnabled(full.Chats, channelID) {
			return errors.New("full fixture channel omitted enabled signatures")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel signatures fixture: %v", err)
	}
	preHistoryChannelID := createMegagroup(t, f.ctx, creator.cmds, "Prehistory fixture")
	if err := fixtureClientCall(t, creator, "channels_togglePreHistoryHidden.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.ChannelsTogglePreHistoryHidden(ctx, &tg.ChannelsTogglePreHistoryHiddenRequest{
			Channel: inputChannel(creator.id, preHistoryChannelID),
			Enabled: true,
		})
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok {
			return fmt.Errorf("toggle fixture prehistory result = %T, want updated channel %d", result, preHistoryChannelID)
		}
		var updated bool
		for _, chat := range updates.Chats {
			if channel, ok := chat.(*tg.Channel); ok && channel.ID == preHistoryChannelID {
				updated = true
				break
			}
		}
		if !updated {
			return fmt.Errorf("toggle fixture prehistory omitted updated channel %d", preHistoryChannelID)
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, preHistoryChannelID))
		if err != nil {
			return err
		}
		channelFull, ok := full.FullChat.(*tg.ChannelFull)
		if !ok || !channelFull.GetHiddenPrehistory() {
			return fmt.Errorf("full fixture channel hidden_prehistory = %T/%v, want true", full.FullChat, ok && channelFull.GetHiddenPrehistory())
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel prehistory fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_editChatAbout.2", func(ctx context.Context, api *tg.Client) error {
		changed, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer:  peerChannel(creator.id, channelID),
			About: "Daily notes",
		})
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("edit fixture channel about returned false")
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		channelFull, ok := full.FullChat.(*tg.ChannelFull)
		if !ok || channelFull.About != "Daily notes" {
			return fmt.Errorf("full channel about = %#v, want Daily notes", full.FullChat)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel about fixture: %v", err)
	}

	for i, offsetID := range []int{2, 1} {
		fixtureName := fmt.Sprintf("messages_getHistory.%d", i+1)
		if err := fixtureClientCall(t, creator, fixtureName, func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     peerChannel(creator.id, channelID),
				OffsetID: offsetID,
				Limit:    50,
			})
			return err
		}); err != nil {
			t.Fatalf("replay history fixture %d: %v", i+1, err)
		}
	}

	replyChannel, createReplyErr := f.store.CreateChannel(f.ctx, creator.id, "Reply fixture group", "", true)
	if createReplyErr != nil {
		t.Fatalf("create reply fixture supergroup: %v", createReplyErr)
	}
	root, _, _, seedReplyErr := f.store.PostChannelMessageAs(f.ctx, replyChannel.ID, creator.id, "reply fixture root", 13030201, nil, 0)
	if seedReplyErr != nil {
		t.Fatalf("seed reply fixture root: %v", seedReplyErr)
	}
	reply, _, _, seedReplyErr := f.store.PostChannelMessageAs(f.ctx, replyChannel.ID, creator.id, "visible fixture reply", 13030202, nil, root.LocalID)
	if seedReplyErr != nil {
		t.Fatalf("seed reply fixture reply: %v", seedReplyErr)
	}
	if err := fixtureClientCall(t, creator, "messages_getReplies.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{
			Peer:  peerChannel(creator.id, replyChannel.ID),
			MsgID: int(root.LocalID),
			Limit: 10,
		})
		if err != nil {
			return err
		}
		messages, ok := result.(*tg.MessagesChannelMessages)
		if !ok {
			return fmt.Errorf("getReplies response = %T, want *tg.MessagesChannelMessages", result)
		}
		if messages.Count != 1 || len(messages.Messages) != 1 {
			return fmt.Errorf("getReplies count/page = %d/%d, want one reply", messages.Count, len(messages.Messages))
		}
		message, ok := messages.Messages[0].(*tg.Message)
		if !ok || message.ID != int(reply.LocalID) || message.Message != "visible fixture reply" {
			return fmt.Errorf("getReplies message = %#v, want direct reply %d", messages.Messages[0], reply.LocalID)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay getReplies fixture: %v", err)
	}

	var groupMessageID int
	var channelMessageID int
	for i, message := range []struct {
		fixture string
		peer    tg.InputPeerClass
		text    string
	}{
		{fixture: "messages_sendMessage.1", peer: peerUser(creator.id, member.id), text: "Hello"},
		{fixture: "messages_sendMessage.2", peer: &tg.InputPeerChat{ChatID: chatID}, text: "Hello"},
		{fixture: "messages_sendMessage.3", peer: peerChannel(creator.id, channelID), text: "Hello followers!"},
	} {
		result := replayFixtureSendMessage(t, creator, message.fixture, message.peer, message.text, int64(13030101+i))
		if message.fixture == "messages_sendMessage.2" {
			outgoing, _, ok := outgoingMessage(t, result, message.text)
			if !ok {
				t.Fatal("group send-message fixture omitted its outgoing message")
			}
			groupMessageID = outgoing.ID
		}
		if message.fixture == "messages_sendMessage.3" {
			updates, ok := result.(*tg.Updates)
			if !ok {
				t.Fatalf("channel post result = %T, want *tg.Updates", result)
			}
			var posted *tg.Message
			for _, update := range updates.Updates {
				created, ok := update.(*tg.UpdateNewChannelMessage)
				if !ok {
					continue
				}
				message, ok := created.Message.(*tg.Message)
				if ok {
					posted = message
					break
				}
			}
			if posted == nil {
				t.Fatal("channel post result omitted updateNewChannelMessage")
			}
			channelMessageID = posted.ID
			if author, present := posted.GetPostAuthor(); !present || author != "Smoke" {
				t.Fatalf("channel post_author = %q, present=%v; want the send-time display name Smoke", author, present)
			}
		}
	}
	if groupMessageID <= 0 {
		t.Fatalf("fixture group message id = %d, want positive", groupMessageID)
	}
	if err := fixtureClientCall(t, creator, "messages_setChatAvailableReactions.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesSetChatAvailableReactions(ctx, &tg.MessagesSetChatAvailableReactionsRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
			AvailableReactions: &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{
				&tg.ReactionEmoji{Emoticon: "❤"},
			}},
		})
		if err == nil && result == nil {
			return errors.New("setChatAvailableReactions returned nil Updates")
		}
		return err
	}); err != nil {
		t.Fatalf("replay set-available-reactions fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_sendReaction.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: groupMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}},
		})
		if err == nil {
			if _, ok := result.(*tg.Updates); !ok {
				return fmt.Errorf("sendReaction result = %T, want *tg.Updates", result)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("replay send-reaction fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getMessagesReactions.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: []int{groupMessageID},
		})
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok || len(updates.Updates) != 1 {
			return fmt.Errorf("getMessagesReactions result = %#v, want one reaction update", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-message-reactions fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getMessageReactionsList.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetMessageReactionsList(ctx, &tg.MessagesGetMessageReactionsListRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: groupMessageID, Limit: 50,
		})
		if err != nil {
			return err
		}
		if result.Count != 1 || len(result.Reactions) != 1 {
			return fmt.Errorf("getMessageReactionsList count/results = %d/%d, want 1/1", result.Count, len(result.Reactions))
		}
		return nil
	}); err != nil {
		t.Fatalf("replay get-message-reactions-list fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_setChatAvailableReactions.channel.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesSetChatAvailableReactions(ctx, &tg.MessagesSetChatAvailableReactionsRequest{
			Peer: peerChannel(creator.id, channelID),
			AvailableReactions: &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{
				&tg.ReactionEmoji{Emoticon: "❤"},
			}},
		})
		if err == nil && result == nil {
			return errors.New("set channel available reactions returned nil Updates")
		}
		return err
	}); err != nil {
		t.Fatalf("replay channel set-available-reactions fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_sendReaction.channel.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: peerChannel(creator.id, channelID), MsgID: channelMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}},
		})
		if err == nil {
			if _, ok := result.(*tg.Updates); !ok {
				return fmt.Errorf("channel sendReaction result = %T, want *tg.Updates", result)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("replay channel send-reaction fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_getMessagesReactions.channel.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
			Peer: peerChannel(creator.id, channelID), ID: []int{channelMessageID},
		})
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok || len(updates.Updates) != 1 {
			return fmt.Errorf("channel getMessagesReactions result = %#v, want one reaction update", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay channel get-message-reactions fixture: %v", err)
	}
	if err := fixtureClientCall(t, creator, "messages_deleteHistory.1", func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesDeleteHistoryRequest{Peer: peerUser(creator.id, member.id)}
		request.SetJustClear(true)
		_, err := api.MessagesDeleteHistory(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("replay delete-history fixture: %v", err)
	}

	for _, part := range []struct {
		fixture string
		index   int
		size    int
	}{
		{fixture: "upload_saveFilePart.1", index: 0, size: 32768},
		{fixture: "upload_saveFilePart.8", index: 7, size: 10239},
	} {
		var saved bool
		err := fixtureClientCall(t, creator, part.fixture, func(ctx context.Context, api *tg.Client) error {
			var err error
			saved, err = api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID:   13030110,
				FilePart: part.index,
				Bytes:    mediaPayload(part.size),
			})
			return err
		})
		if err != nil {
			t.Fatalf("replay upload fixture %s: %v", part.fixture, err)
		}
		if !saved {
			t.Errorf("upload fixture %s returned false, want true", part.fixture)
		}
	}

	var photoErr error
	err = fixtureClientCall(t, creator, "messages_sendMedia.3", func(ctx context.Context, api *tg.Client) error {
		_, photoErr = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(creator.id, member.id),
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: 13030110, Parts: 8, Name: "REDACTED.jpg", MD5Checksum: "00000000000000000000000000000000",
			}},
			Message:  "",
			RandomID: 13030111,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("replay photo fixture request: %v", err)
	}
	assertRPCError(t, photoErr, "MEDIA_INVALID")

	var typingResult bool
	var typingErr error
	err = fixtureClientCall(t, creator, "messages_setTyping.1", func(ctx context.Context, api *tg.Client) error {
		typingResult, typingErr = api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
			Peer:   &tg.InputPeerChat{ChatID: chatID},
			Action: &tg.SendMessageTypingAction{},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("replay basic-group typing fixture: %v", err)
	}
	if typingErr != nil {
		t.Fatalf("basic-group typing request: %v", typingErr)
	}
	if !typingResult {
		t.Fatal("basic-group typing returned false, want true")
	}

	var exported tg.ExportedChatInviteClass
	if err := fixtureClientCall(t, creator, "messages_exportChatInvite.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		exported, err = api.MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{Peer: peerChannel(creator.id, channelID)})
		return err
	}); err != nil {
		t.Fatalf("replay export invite fixture: %v", err)
	}
	if _, ok := exported.(*tg.ChatInviteExported); !ok {
		t.Fatalf("export invite result = %T, want *tg.ChatInviteExported", exported)
	}

	var usernameAvailable bool
	if err := fixtureClientCall(t, creator, "channels_checkUsername.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		usernameAvailable, err = api.ChannelsCheckUsername(ctx, &tg.ChannelsCheckUsernameRequest{
			Channel:  inputChannel(creator.id, channelID),
			Username: "clientfixture1303",
		})
		return err
	}); err != nil {
		t.Fatalf("replay check-username fixture: %v", err)
	}
	if !usernameAvailable {
		t.Error("check-username fixture returned false, want true")
	}

	var invited *tg.MessagesInvitedUsers
	if err := fixtureClientCall(t, creator, "channels_inviteToChannel.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		invited, err = api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
			Channel: inputChannel(creator.id, channelID),
			Users:   []tg.InputUserClass{inputUser(creator.id, member.id)},
		})
		return err
	}); err != nil {
		t.Fatalf("replay channel-invite fixture: %v", err)
	}
	if len(invited.MissingInvitees) != 0 {
		t.Errorf("invite fixture returned %d missing invitees, want none", len(invited.MissingInvitees))
	}
	for i := 1; i <= 5; i++ {
		fixtureName := fmt.Sprintf("channels_getFullChannel.%d", i)
		if err := fixtureClientCall(t, creator, fixtureName, func(ctx context.Context, api *tg.Client) error {
			_, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
			return err
		}); err != nil {
			t.Fatalf("replay full-channel fixture %d: %v", i, err)
		}
	}

	for _, test := range []struct {
		fixture string
		message string
		random  int64
	}{
		{fixture: "messages_sendMedia.1", message: "Description stuff", random: 1048901},
		{fixture: "messages_sendMedia.2", message: "", random: 1048902},
	} {
		request := smokeClientPollRequest(chatID, test.message, test.random)
		var sendErr error
		var result tg.UpdatesClass
		start := creator.requests.count()
		err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, sendErr = api.MessagesSendMedia(ctx, request)
			if fixtureErr := creator.requests.matchFixtureSince(start, test.fixture); fixtureErr != nil {
				return fixtureErr
			}
			return nil
		})
		if err != nil {
			t.Errorf("replay poll request fixture %s: %v", test.fixture, err)
			continue
		}
		if sendErr != nil {
			t.Errorf("server rejected poll request fixture %s: %v", test.fixture, sendErr)
			continue
		}
		if err := assertSmokeClientPollSend(result, test.message); err != nil {
			t.Errorf("poll response for fixture %s: %v", test.fixture, err)
		}
	}

	replayCapturedPollVoteAndClose(t, creator, member, chatID)

	const answerURL = "https://poll-answer.invalid/example?a=1"
	request := smokeClientPollRequest(chatID, "Answer link fixture", 1048903)
	pollMedia, ok := request.Media.(*tg.InputMediaPoll)
	if !ok {
		t.Fatalf("fixture request media = %T, want inputMediaPoll", request.Media)
	}
	answer, ok := pollMedia.Poll.Answers[0].(*tg.InputPollAnswer)
	if !ok {
		t.Fatalf("fixture poll answer = %T, want inputPollAnswer", pollMedia.Poll.Answers[0])
	}
	answer.SetMedia(&tg.InputMediaWebPage{URL: answerURL})
	var linkedPoll tg.UpdatesClass
	if err := fixtureClientCall(t, creator, "messages_sendMedia.4", func(ctx context.Context, api *tg.Client) error {
		var err error
		linkedPoll, err = api.MessagesSendMedia(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("replay poll answer web-page fixture: %v", err)
	}
	updates, ok := linkedPoll.(*tg.Updates)
	if !ok {
		t.Fatalf("poll answer web-page fixture result = %T, want *tg.Updates", linkedPoll)
	}
	for _, update := range updates.Updates {
		created, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := created.Message.(*tg.Message)
		if !ok {
			t.Fatalf("poll answer web-page fixture message = %T, want *tg.Message", created.Message)
		}
		assertSmokePollAnswerWebPage(t, "poll answer web-page fixture", message, answerURL)
		for _, test := range []struct {
			fixture string
			peer    tg.InputPeerClass
			period  int
		}{
			{fixture: "messages_setHistoryTTL.1", peer: peerUser(creator.id, member.id), period: 86400},
			{fixture: "messages_setHistoryTTL.2", peer: &tg.InputPeerChat{ChatID: chatID}, period: 604800},
			{fixture: "messages_setHistoryTTL.3", peer: peerChannel(creator.id, channelID), period: 2592000},
		} {
			if err := fixtureClientCall(t, creator, test.fixture, func(ctx context.Context, api *tg.Client) error {
				_, err := api.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
					Peer: test.peer, Period: test.period,
				})
				return err
			}); err != nil {
				t.Fatalf("replay history-TTL fixture %s: %v", test.fixture, err)
			}
		}
		if err := fixtureClientCall(t, creator, "messages_deleteChat.1", func(ctx context.Context, api *tg.Client) error {
			deleted, err := api.MessagesDeleteChat(ctx, chatID)
			if err != nil {
				return err
			}
			if !deleted {
				return errors.New("deleteChat returned false")
			}
			return nil
		}); err != nil {
			t.Fatalf("replay delete-chat fixture: %v", err)
		}
		if err := fixtureClientCall(t, creator, "messages_sendMedia.sticker.1", func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer: peerUser(creator.id, member.id),
				Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
					ID: stickerDocument.ID, AccessHash: stickerDocument.AccessHash, FileReference: stickerDocument.FileReference,
				}},
				Message:  "Catalogue sticker fixture",
				RandomID: 13030301,
			})
			return err
		}); err != nil {
			t.Fatalf("replay catalogue sticker sendMedia fixture: %v", err)
		}
		return
	}
	t.Fatal("poll answer web-page fixture response omitted updateNewMessage")
}

func channelSignaturesEnabled(chats []tg.ChatClass, channelID int64) bool {
	for _, chat := range chats {
		channel, ok := chat.(*tg.Channel)
		if ok && channel.ID == channelID {
			return channel.GetSignatures()
		}
	}
	return false
}

func replayCapturedPollVoteAndClose(t *testing.T, creator, member *smokeClient, chatID int64) {
	t.Helper()

	const question = "MAIN1999client-shapepoll"
	const pollRequestID = int64(1048991)
	var pollID int64
	var sendResult tg.UpdatesClass
	if err := creator.call(creator.lifecycle.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		sendResult, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
			Media: &tg.InputMediaPoll{Poll: tg.Poll{
				ID:             pollRequestID,
				PublicVoters:   true,
				MultipleChoice: true,
				ShuffleAnswers: true,
				Question:       tg.TextWithEntities{Text: question},
				Answers: []tg.PollAnswerClass{
					&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Yes"}},
					&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "No"}},
				},
			}},
			RandomID: pollRequestID + 1,
		})
		return err
	}); err != nil {
		t.Fatalf("send fixture vote poll: %v", err)
	}
	updates, ok := sendResult.(*tg.Updates)
	if !ok {
		t.Fatalf("fixture vote poll result = %T, want *tg.Updates", sendResult)
	}
	var pollMessage *tg.Message
	for _, update := range updates.Updates {
		created, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := created.Message.(*tg.Message)
		if !ok {
			t.Fatalf("fixture vote poll message = %T, want *tg.Message", created.Message)
		}
		poll, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || poll.Poll.Question.Text != question {
			t.Fatalf("fixture vote poll media = %#v, want question %q", message.Media, question)
		}
		pollID = poll.Poll.ID
		answer, ok := poll.Poll.Answers[0].(*tg.PollAnswer)
		if !ok || len(answer.Option) == 0 {
			t.Fatalf("fixture vote poll first answer = %#v, want server option", poll.Poll.Answers[0])
		}
		pollMessage = message
		break
	}
	if pollMessage == nil {
		t.Fatal("fixture vote poll response omitted updateNewMessage")
	}
	var memberPollMessage *tg.Message
	var firstOption []byte
	var history tg.MessagesMessagesClass
	if err := fixtureClientCall(t, member, "messages_getHistory.second-account.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  &tg.InputPeerChat{ChatID: chatID},
			Limit: 50,
		})
		if err != nil {
			return err
		}
		messages, ok := history.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("second-account history = %T, want *tg.MessagesMessages", history)
		}
		for _, candidate := range messages.Messages {
			message, ok := candidate.(*tg.Message)
			if !ok {
				continue
			}
			poll, ok := message.Media.(*tg.MessageMediaPoll)
			if !ok || poll.Poll.ID != pollID || poll.Poll.Question.Text != question {
				continue
			}
			answer, ok := poll.Poll.Answers[0].(*tg.PollAnswer)
			if !ok || len(answer.Option) == 0 {
				return fmt.Errorf("second-account poll first answer = %#v, want server option", poll.Poll.Answers[0])
			}
			memberPollMessage, firstOption = message, answer.Option
			return nil
		}
		return fmt.Errorf("second-account history omitted poll %d", pollID)
	}); err != nil {
		t.Fatalf("replay second-account history fixture: %v", err)
	}

	var voteResult tg.UpdatesClass
	if err := fixtureClientCall(t, member, "messages_sendVote.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		voteResult, err = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: memberPollMessage.ID, Options: [][]byte{firstOption},
		})
		return err
	}); err != nil {
		t.Fatalf("replay poll vote fixture: %v", err)
	}
	voteUpdates, ok := voteResult.(*tg.Updates)
	if !ok {
		t.Fatalf("fixture poll vote result = %T, want *tg.Updates", voteResult)
	}
	var vote *tg.UpdateMessagePoll
	for _, update := range voteUpdates.Updates {
		if result, ok := update.(*tg.UpdateMessagePoll); ok {
			vote = result
			break
		}
	}
	if vote == nil || vote.PollID != pollID || len(vote.Results.Results) != 2 || vote.Results.Results[0].Voters != 1 {
		t.Fatalf("fixture poll vote update = %+v, want poll %d with one first-option vote", vote, pollID)
	}
	voted := recvOrCtx(t, member.lifecycle.ctx, member.push.editMsg, "second-account poll vote")
	votedPoll, ok := voted.Media.(*tg.MessageMediaPoll)
	if !ok || votedPoll.Poll.ID != pollID || votedPoll.Poll.Closed || len(votedPoll.Results.Results) != 2 || votedPoll.Results.Results[0].Voters != 1 {
		t.Fatalf("second-account voted poll = %+v, want open poll %d with one first-option vote", voted, pollID)
	}

	closePoll := tg.Poll{
		ID:             pollID,
		PublicVoters:   true,
		MultipleChoice: true,
		ShuffleAnswers: true,
		Question:       tg.TextWithEntities{Text: question},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Yes"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "No"}},
		},
	}
	closePoll.SetClosed(true)
	var closeResult tg.UpdatesClass
	if err := fixtureClientCall(t, creator, "messages_editMessage.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		closeResult, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
			ID:   pollMessage.ID,
			Media: &tg.InputMediaPoll{
				Poll: closePoll,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("replay close-poll fixture: %v", err)
	}
	if _, ok := closeResult.(*tg.Updates); !ok {
		t.Fatalf("fixture close poll result = %T, want *tg.Updates", closeResult)
	}
	closed := recvOrCtx(t, member.lifecycle.ctx, member.push.editMsg, "second-account poll close")
	closedPoll, ok := closed.Media.(*tg.MessageMediaPoll)
	if !ok || closed.ID != memberPollMessage.ID || closedPoll.Poll.ID != pollID || !closedPoll.Poll.Closed ||
		len(closedPoll.Results.Results) != 2 || closedPoll.Results.Results[0].Voters != 1 {
		t.Fatalf("second-account closed poll = %+v, want closed poll %d with one first-option vote", closed, pollID)
	}
}

func replayFixtureSendMessage(t *testing.T, client *smokeClient, fixtureName string, peer tg.InputPeerClass, message string, randomID int64) tg.UpdatesClass {
	t.Helper()
	request := &tg.MessagesSendMessageRequest{Peer: peer, Message: message, RandomID: randomID}
	request.SetClearDraft(true)
	var result tg.UpdatesClass
	if err := fixtureClientCall(t, client, fixtureName, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesSendMessage(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("replay text fixture %s: %v", fixtureName, err)
	}
	if _, ok := result.(*tg.Updates); !ok {
		t.Fatalf("text fixture %s result = %T, want *tg.Updates", fixtureName, result)
	}
	return result
}

func fixtureClientCall(t *testing.T, client *smokeClient, fixtureName string, call func(context.Context, *tg.Client) error) error {
	t.Helper()
	start := client.requests.count()
	return client.call(client.lifecycle.ctx, func(ctx context.Context, api *tg.Client) error {
		callErr := call(ctx, api)
		if err := client.requests.matchFixtureSince(start, fixtureName); err != nil {
			return err
		}
		return callErr
	})
}

func smokeClientPollRequest(chatID int64, message string, randomID int64) *tg.MessagesSendMediaRequest {
	return &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerChat{ChatID: chatID},
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			ID:             randomID,
			PublicVoters:   true,
			MultipleChoice: true,
			OpenAnswers:    true,
			ShuffleAnswers: true,
			Question:       tg.TextWithEntities{Text: "Question 1"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Option 1"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Option 2😁"}},
			},
		}},
		Message:  message,
		RandomID: randomID + 10,
	}
}

func assertSmokeClientPollSend(result tg.UpdatesClass, wantMessage string) error {
	updates, ok := result.(*tg.Updates)
	if !ok {
		return fmt.Errorf("sendMedia result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		created, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := created.Message.(*tg.Message)
		if !ok {
			return fmt.Errorf("new poll message = %T, want *tg.Message", created.Message)
		}
		if message.Message != wantMessage {
			return fmt.Errorf("poll description = %q, want %q", message.Message, wantMessage)
		}
		if media, ok := message.Media.(*tg.MessageMediaPoll); !ok || media.Poll.ID <= 0 || media.Poll.Question.Text != "Question 1" {
			return fmt.Errorf("poll media = %#v, want the captured question", message.Media)
		}
		return nil
	}
	return errors.New("sendMedia response omitted updateNewMessage")
}
