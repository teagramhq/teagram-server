package erasureledger_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// TestSelfDeletePreservesExactlyOneCopy is the accepted
// recovery case: a delete-for-self for owner 1, local 7 replays to that copy
// only. The record must preserve exactly that copy, must not
// become a revoke, and must not be able to name owner 2's copy.
func TestSelfDeletePreservesExactlyOneCopy(t *testing.T) {
	t.Parallel()
	self, err := erasureledger.NewSelfDelete(1, 7)
	if err != nil {
		t.Fatalf("NewSelfDelete: %v", err)
	}
	data, err := erasureledger.Encode(record(t, erasureledger.KindMessageCopies, 1, 1, self))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := erasureledger.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := payloadAs[erasureledger.MessageCopies](t, out.Payload)
	if len(got.Copies) != 1 {
		t.Fatalf("copy set = %+v, want exactly one copy", got.Copies)
	}
	if got.Copies[0] != (erasureledger.MessageCopy{OwnerID: 1, LocalID: 7}) {
		t.Errorf("copy = %+v, want owner 1 local 7", got.Copies[0])
	}
	for _, c := range got.Copies {
		if c.OwnerID == 2 {
			t.Errorf("a self-delete record names owner 2's copy: %+v", got.Copies)
		}
	}
	// A self-delete body is one set and nothing else: there is no
	// flag that can turn it into a revoke for everyone, and no field that can
	// name a scope wider than the copies it lists.
	typ := reflect.TypeFor[erasureledger.MessageCopies]()
	if typ.NumField() != 1 {
		t.Errorf("MessageCopies has %d fields, want the copy set only", typ.NumField())
	}
	if f := typ.Field(0); f.Name != "Copies" {
		t.Errorf("MessageCopies field = %s, want Copies", f.Name)
	}
	if typ := reflect.TypeFor[erasureledger.MessageCopy](); typ.NumField() != 2 {
		t.Errorf("MessageCopy has %d fields, want owner and local id", typ.NumField())
	}
}

// TestRevokeScopeIsExactlyTheRecordedSet covers the 1:1 revoke (both copies)
// and the group revoke (the members present at commit, with a removed
// member's frozen copy absent). Replay applies the recorded set, so the
// recorded set is the whole authorization: it cannot widen and cannot narrow.
func TestRevokeScopeIsExactlyTheRecordedSet(t *testing.T) {
	t.Parallel()
	peer, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 7},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies: %v", err)
	}
	group, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 3, LocalID: 41},
		erasureledger.MessageCopy{OwnerID: 4, LocalID: 41},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies: %v", err)
	}
	cases := []struct {
		name   string
		in     erasureledger.MessageCopies
		want   []erasureledger.MessageCopy
		absent int64
	}{
		{name: "peer revoke is both copies", in: peer,
			want: []erasureledger.MessageCopy{{OwnerID: 1, LocalID: 7}, {OwnerID: 2, LocalID: 7}}},
		{name: "group revoke is the commit-time set", in: group,
			want: []erasureledger.MessageCopy{
				{OwnerID: 1, LocalID: 7}, {OwnerID: 3, LocalID: 41}, {OwnerID: 4, LocalID: 41},
			},
			// A member removed before commit has a frozen copy, and the
			// record omits it: replay must not reach it.
			absent: 9,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := erasureledger.Encode(record(t, erasureledger.KindMessageCopies, 1, 4, tc.in))
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			out, err := erasureledger.Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			got := payloadAs[erasureledger.MessageCopies](t, out.Payload)
			if !reflect.DeepEqual(got.Copies, tc.want) {
				t.Errorf("copies = %+v, want %+v", got.Copies, tc.want)
			}
			if tc.absent != 0 {
				for _, c := range got.Copies {
					if c.OwnerID == tc.absent {
						t.Errorf("the removed member's frozen copy is present: %+v", got.Copies)
					}
				}
			}
		})
	}
}

// TestChannelPostCopiesStayChannelScoped pins the two namespaces apart: a
// channel post names a channel and a local id in that channel's space, never
// an owner, so a channel post record cannot reach an account's copy set.
func TestChannelPostCopiesStayChannelScoped(t *testing.T) {
	t.Parallel()
	posts, err := erasureledger.NewChannelPostCopies(erasureledger.ChannelPost{ChannelID: 3000000001, LocalID: 41})
	if err != nil {
		t.Fatalf("NewChannelPostCopies: %v", err)
	}
	data, err := erasureledger.Encode(record(t, erasureledger.KindChannelPosts, 1, 1, posts))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := erasureledger.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := payloadAs[erasureledger.ChannelPostCopies](t, out.Payload)
	if !reflect.DeepEqual(got.Posts, posts.Posts) {
		t.Errorf("posts = %+v, want %+v", got.Posts, posts.Posts)
	}
	typ := reflect.TypeFor[erasureledger.ChannelPost]()
	names := fieldNames(typ)
	if !slices.Contains(names, "ChannelID") || slices.Contains(names, "OwnerID") {
		t.Errorf("ChannelPost fields = %v, want a channel-scoped identity", names)
	}
}

// TestGalleryClearKeepsExactTuplesAndRevision is the accepted gallery case:
// a clear of B removes exactly B and promotes nothing, keeps the per-owner
// revision it cleared at, and cannot name a replacement.
func TestGalleryClearKeepsExactTuplesAndRevision(t *testing.T) {
	t.Parallel()
	cleared, err := erasureledger.NewGalleryDelete(5, 9,
		erasureledger.GalleryEntry{FileID: 203, ClientFileID: 88},
		erasureledger.GalleryEntry{FileID: 204, ClientFileID: 89},
	)
	if err != nil {
		t.Fatalf("NewGalleryDelete: %v", err)
	}
	data, err := erasureledger.Encode(record(t, erasureledger.KindGalleryDelete, 2, 3, cleared))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := erasureledger.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := payloadAs[erasureledger.GalleryDelete](t, out.Payload)
	if got.OwnerID != 5 {
		t.Errorf("owner = %d, want 5", got.OwnerID)
	}
	if got.Revision != 9 {
		t.Errorf("revision = %d, want 9 exactly (no clamp, no widen)", got.Revision)
	}
	want := []erasureledger.GalleryEntry{{FileID: 203, ClientFileID: 88}, {FileID: 204, ClientFileID: 89}}
	if !reflect.DeepEqual(got.Entries, want) {
		t.Errorf("deleted tuples = %+v, want %+v", got.Entries, want)
	}
	// The record names what was deleted, the owner it belonged to, and the
	// revision it cleared at. Nothing else: no current-selection pointer, no
	// promoted entry, no added entry, so clear-of-B over a restored older A
	// cannot turn into a promotion.
	names := fieldNames(reflect.TypeFor[erasureledger.GalleryDelete]())
	wantFields := []string{"OwnerID", "Entries", "Revision"}
	if !slices.Equal(names, wantFields) {
		t.Errorf("GalleryDelete fields = %v, want %v", names, wantFields)
	}
	entryFields := fieldNames(reflect.TypeFor[erasureledger.GalleryEntry]())
	if !slices.Equal(entryFields, []string{"FileID", "ClientFileID"}) {
		t.Errorf("GalleryEntry fields = %v, want the two deleted identifiers", entryFields)
	}
}

// TestTerminalReceiptIdentityIsMinimal covers the accepted requirement that a
// replayer can produce a terminal receipt the restored database never held,
// from the accepted minimal identifiers alone.
func TestTerminalReceiptIdentityIsMinimal(t *testing.T) {
	t.Parallel()
	absent, err := erasureledger.NewReceiptDeleted(5, 88, erasureledger.NoFileID)
	if err != nil {
		t.Fatalf("NewReceiptDeleted: %v", err)
	}
	survivor, err := erasureledger.NewReceiptDeleted(5, 88, 203)
	if err != nil {
		t.Fatalf("NewReceiptDeleted: %v", err)
	}
	cases := []struct {
		name  string
		in    erasureledger.ReceiptTerminal
		file  int64
		state erasureledger.ReceiptState
	}{
		{name: "receipt absent from the restored database", in: absent, file: erasureledger.NoFileID, state: erasureledger.ReceiptDeleted},
		{name: "receipt names the file it terminalized", in: survivor, file: 203, state: erasureledger.ReceiptDeleted},
		{name: "complete upload is terminal and dedupable",
			in: func() erasureledger.ReceiptTerminal {
				r, err := erasureledger.NewReceiptTerminal(5, 88, erasureledger.ReceiptComplete, 203)
				if err != nil {
					t.Fatalf("NewReceiptTerminal: %v", err)
				}
				return r
			}(), file: 203, state: erasureledger.ReceiptComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := erasureledger.Encode(record(t, erasureledger.KindReceiptTerminal, 1, 1, tc.in))
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			out, err := erasureledger.Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			got := payloadAs[erasureledger.ReceiptTerminal](t, out.Payload)
			if got.OwnerID != 5 || got.ClientFileID != 88 {
				t.Errorf("identity = (%d,%d), want (5,88)", got.OwnerID, got.ClientFileID)
			}
			if got.State != tc.state {
				t.Errorf("state = %v, want %v", got.State, tc.state)
			}
			if got.FileID != tc.file {
				t.Errorf("file id = %d, want %d", got.FileID, tc.file)
			}
		})
	}
	// The identity is the durable row's primary key plus the terminal state
	// and, when one existed, the file. Request size, part count, payload
	// digest and media mode stay in the database: they are enough to
	// fingerprint a deleted photo and a replayer does not need them.
	names := fieldNames(reflect.TypeFor[erasureledger.ReceiptTerminal]())
	wantFields := []string{"OwnerID", "ClientFileID", "State", "FileID"}
	if !slices.Equal(names, wantFields) {
		t.Errorf("ReceiptTerminal fields = %v, want %v", names, wantFields)
	}
}

// TestTerminalStatesAreOneWay pins the enumerated lattices: every accepted
// value is terminal, and there is no value meaning pending, live, restored,
// or un-completed.
func TestTerminalStatesAreOneWay(t *testing.T) {
	t.Parallel()
	for _, s := range []erasureledger.ReceiptState{erasureledger.ReceiptComplete, erasureledger.ReceiptDeleted} {
		if !s.Terminal() {
			t.Errorf("ReceiptState %v is not terminal", s)
		}
	}
	for _, s := range []erasureledger.ReceiptState{0, 3, 255} {
		if s.Terminal() {
			t.Errorf("ReceiptState %v accepted as terminal", s)
		}
	}
	for _, l := range []erasureledger.EpochLevel{erasureledger.EpochEstablished, erasureledger.EpochCompleted} {
		if !l.Valid() {
			t.Errorf("EpochLevel %v is not valid", l)
		}
	}
	for _, l := range []erasureledger.EpochLevel{0, 3, 255} {
		if l.Valid() {
			t.Errorf("EpochLevel %v accepted", l)
		}
	}
	for _, c := range []erasureledger.RandomClass{erasureledger.RandomClassChannel, erasureledger.RandomClassPoll} {
		if !c.Valid() {
			t.Errorf("RandomClass %v is not valid", c)
		}
	}
	if erasureledger.RandomClass(0).Valid() || erasureledger.RandomClass(3).Valid() {
		t.Error("an unassigned random allocator class is accepted")
	}
}

// TestEveryKindIsMonotone is the vocabulary's ordering contract: every kind is
// classified as a set-to-deleted, tombstone, maximum, or immutable binding.
// Order-independent, idempotent replay rests on that, and an unclassified kind
// is a kind whose replay order would matter.
func TestEveryKindIsMonotone(t *testing.T) {
	t.Parallel()
	classes := map[erasureledger.Monotone]bool{
		erasureledger.MonotoneSetToDeleted: true,
		erasureledger.MonotoneTombstone:    true,
		erasureledger.MonotoneMaximum:      true,
		erasureledger.MonotoneBinding:      true,
	}
	specs := erasureledger.KindSpecs()
	if len(specs) == 0 {
		t.Fatal("the vocabulary is empty")
	}
	kinds := make([]erasureledger.Kind, 0, len(specs))
	for _, spec := range specs {
		if !classes[spec.Monotone] {
			t.Errorf("kind %v has unclassified monotone class %v", spec.Kind, spec.Monotone)
		}
		if spec.Scope == "" {
			t.Errorf("kind %v has no documented scope boundary", spec.Kind)
		}
		if spec.Name() == "" {
			t.Errorf("kind %v has no payload type name", spec.Kind)
		}
		known, ok := erasureledger.LookupKind(spec.Kind)
		if !ok || known.Monotone != spec.Monotone {
			t.Errorf("LookupKind(%v) = %+v, ok=%v", spec.Kind, known, ok)
		}
		kinds = append(kinds, spec.Kind)
	}
	if !slices.IsSorted(kinds) {
		t.Errorf("kind registry is not sorted: %v", kinds)
	}
	if len(kinds) != len(slices.Compact(slices.Clone(kinds))) {
		t.Errorf("kind registry has duplicates: %v", kinds)
	}
	for _, k := range []erasureledger.Kind{0, 12, 100, 65535} {
		if _, ok := erasureledger.LookupKind(k); ok {
			t.Errorf("LookupKind(%v) reports a kind this binary is meant to reject", k)
		}
	}
}

// TestCanonicalFormIsOrderIndependent is the order-independence half of
// the replay contract: the same set, stated in any order, is one record.
// Idempotence follows from the monotone classes plus this identity.
func TestCanonicalFormIsOrderIndependent(t *testing.T) {
	t.Parallel()
	forward, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 3, LocalID: 41},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies: %v", err)
	}
	shuffled, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 3, LocalID: 41},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies shuffled: %v", err)
	}
	dup, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 3, LocalID: 41},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies duplicated: %v", err)
	}
	a, err := erasureledger.Encode(record(t, erasureledger.KindMessageCopies, 1, 1, forward))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i, p := range []erasureledger.Payload{shuffled, dup} {
		b, err := erasureledger.Encode(record(t, erasureledger.KindMessageCopies, 1, 1, p))
		if err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("copy set %d encodes differently from the same set: %x vs %x", i, a, b)
		}
	}

	// The same identity for exclusions and gallery entries.
	idsA, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassChannel, 997852516352, 11, 42)
	if err != nil {
		t.Fatalf("NewRandomExclusion: %v", err)
	}
	idsB, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassChannel, 42, 997852516352, 11, 42)
	if err != nil {
		t.Fatalf("NewRandomExclusion again: %v", err)
	}
	if !reflect.DeepEqual(idsA, idsB) {
		t.Errorf("exclusion sets differ: %+v vs %+v", idsA, idsB)
	}
	entriesA, err := erasureledger.NewGalleryDelete(5, 9,
		erasureledger.GalleryEntry{FileID: 204, ClientFileID: 89},
		erasureledger.GalleryEntry{FileID: 203, ClientFileID: 88})
	if err != nil {
		t.Fatalf("NewGalleryDelete: %v", err)
	}
	entriesB, err := erasureledger.NewGalleryDelete(5, 9,
		erasureledger.GalleryEntry{FileID: 203, ClientFileID: 88},
		erasureledger.GalleryEntry{FileID: 204, ClientFileID: 89})
	if err != nil {
		t.Fatalf("NewGalleryDelete again: %v", err)
	}
	if !reflect.DeepEqual(entriesA, entriesB) {
		t.Errorf("gallery tuples differ: %+v vs %+v", entriesA, entriesB)
	}
}

// TestReservationIsAMaximumNotARollback states the ceiling's meaning
// on the wire: two reservations for one allocator are both readable, and the
// vocabulary gives the reader no way to express lowering a ceiling. A
// restore takes the highest ceiling it sees; that rule is admission's, and the
// codec's job is to keep the ceiling exact and in width.
func TestReservationIsAMaximumNotARollback(t *testing.T) {
	t.Parallel()
	name, err := erasureledger.ValidateAllocator("users_id_seq")
	if err != nil {
		t.Fatalf("ValidateAllocator: %v", err)
	}
	low, err := erasureledger.NewReservation(name, 1000)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	high, err := erasureledger.NewReservation(name, 1<<40)
	if err != nil {
		t.Fatalf("NewReservation high: %v", err)
	}
	for _, r := range []erasureledger.Reservation{low, high} {
		data, err := erasureledger.Encode(record(t, erasureledger.KindReservation, 1, 1, r))
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out, err := erasureledger.Decode(data)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got := payloadAs[erasureledger.Reservation](t, out.Payload)
		if got.Ceiling != r.Ceiling || got.Allocator != r.Allocator {
			t.Errorf("reservation = %+v, want %+v", got, r)
		}
	}
	// The schema-identifier rule is what keeps the allocator name from
	// carrying anything else.
	for _, bad := range []string{"", "Users", "users id", "users-id", "users\nseq", "users;drop", "users_id_seq "} {
		if _, err := erasureledger.ValidateAllocator(bad); !errors.Is(err, erasureledger.ErrRejected) {
			t.Errorf("ValidateAllocator(%q) = %v, want rejection", bad, err)
		}
	}
	// A real schema name that mentions the phone-code table is legal:
	// the identifier is a schema name, not user data.
	if _, err := erasureledger.ValidateAllocator("phone_codes_id_seq"); err != nil {
		t.Errorf("ValidateAllocator(phone_codes_id_seq) = %v, want acceptance", err)
	}
	// Ceiling width is int64 and positive; zero is not a reserved ceiling.
	if _, err := erasureledger.NewReservation(name, -1); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("negative ceiling accepted: %v", err)
	}
}

// TestEpochCompletionIsBoundToItsLineage is the MAIN-1362 addendum point:
// completion evidence carries the lineage, so a shared epoch number from a
// different lineage is not completion.
func TestEpochCompletionIsBoundToItsLineage(t *testing.T) {
	t.Parallel()
	completed, err := erasureledger.NewEpoch(3, lineageID(0xa0), erasureledger.EpochCompleted)
	if err != nil {
		t.Fatalf("NewEpoch: %v", err)
	}
	other, err := erasureledger.NewEpoch(3, lineageID(0xb0), erasureledger.EpochCompleted)
	if err != nil {
		t.Fatalf("NewEpoch other: %v", err)
	}
	dataA, err := erasureledger.Encode(record(t, erasureledger.KindEpoch, 3, 1, completed))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dataB, err := erasureledger.Encode(record(t, erasureledger.KindEpoch, 3, 1, other))
	if err != nil {
		t.Fatalf("Encode other: %v", err)
	}
	if bytes.Equal(dataA, dataB) {
		t.Fatal("two lineages encode to the same completion record")
	}
	out, err := erasureledger.Decode(dataA)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := payloadAs[erasureledger.Epoch](t, out.Payload)
	if got.Lineage != lineageID(0xa0) {
		t.Errorf("lineage = %v, want the completed lineage", got.Lineage)
	}
	if got.Number != 3 || got.Level != erasureledger.EpochCompleted {
		t.Errorf("epoch = (%d,%v), want (3,completed)", got.Number, got.Level)
	}
}

// TestSequenceIsOneBased pins the enumeration contract: sequences start at 1,
// so a gap in a walk is a fact a reader can detect, and epoch 0 is not a
// real epoch a record can hide behind.
func TestSequenceIsOneBased(t *testing.T) {
	t.Parallel()
	acct, err := erasureledger.NewAccount(1)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	for _, seq := range []int64{0, -1} {
		if _, err := erasureledger.NewRecord(erasureledger.KindAccount, 1, streamID(1), seq, opKey(1), acct); !errors.Is(err, erasureledger.ErrRejected) {
			t.Errorf("sequence %d accepted", seq)
		}
	}
	for _, epoch := range []int64{0, -1} {
		if _, err := erasureledger.NewRecord(erasureledger.KindAccount, epoch, streamID(1), 1, opKey(1), acct); !errors.Is(err, erasureledger.ErrRejected) {
			t.Errorf("epoch %d accepted", epoch)
		}
	}
}

// fieldNames lists a struct type's field names in declaration order.
func fieldNames(typ reflect.Type) []string {
	names := make([]string, 0, typ.NumField())
	for f := range typ.Fields() {
		names = append(names, f.Name)
	}
	return names
}
