package e2e_test

import (
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

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tdp"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const clientFixtureDirectory = "../fixtures/client/layer-228"

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
	case "id", "chat_id", "channel_id", "user_id", "access_hash", "random_id":
		return true
	default:
		return false
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
	if fixture.Layer != 228 || fixture.Client != "Teagram Desktop 7.0.9" || fixture.Source == "" || fixture.Method == "" || fixture.Request == nil {
		return clientRequestFixture{}, fmt.Errorf("invalid client request fixture metadata in %q", name)
	}
	if !strings.HasPrefix(fixture.Source, "captures/") {
		return clientRequestFixture{}, fmt.Errorf("client request fixture %q has invalid capture path %q", name, fixture.Source)
	}
	if _, err := os.Stat(filepath.Join(clientFixtureDirectory, fixture.Source)); err != nil {
		return clientRequestFixture{}, fmt.Errorf("read source capture for fixture %q: %w", name, err)
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
		"channels_getFullChannel.1",
		"channels_getFullChannel.2",
		"channels_getFullChannel.3",
		"channels_getFullChannel.4",
		"channels_getFullChannel.5",
		"messages_getDialogs.1",
		"messages_getDialogs.2",
		"messages_getDialogs.3",
		"messages_getFullChat.1",
		"messages_getHistory.1",
		"messages_getHistory.2",
		"messages_sendMedia.1",
		"messages_sendMedia.2",
	} {
		t.Run(name, func(t *testing.T) {
			fixture, err := readClientRequestFixture(name)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.Layer != 228 || fixture.Client != "Teagram Desktop 7.0.9" || fixture.Source == "" || fixture.Method == "" || fixture.Request == nil {
				t.Fatalf("invalid client request fixture metadata: %+v", fixture)
			}
		})
	}
}

func testSmokeClientRequestFixtures(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneMember = "+15551048901", "+15551048902"
	seedPhoneUsers(t, f.ctx, f.store, phoneCreator, phoneMember)
	creator := newSmokeClientWithRequests(t, f, "Client fixture creator", phoneCreator, &clientRequestRecorder{})
	member := newSmokeClient(t, f, "Client fixture member", phoneMember)

	var chatID int64
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		created, err := api.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Client fixture group",
			Users: []tg.InputUserClass{inputUser(creator.id, member.id)},
		})
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

	channelID := createBroadcastChannel(t, f.ctx, creator.cmds, "Client fixture channel")
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
