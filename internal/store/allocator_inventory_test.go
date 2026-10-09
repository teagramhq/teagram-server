package store_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/store"
)

// Allocator inventory (MAIN-1451): an executable classification of every id
// this schema hands out, read from a freshly migrated disposable database.
//
// It answers three questions for every allocator: how the next value is
// produced, how wide the value is when it is served, and what stops it
// running out. Sequence-backed allocators are enumerated from the schema, so a
// sequence added later fails the inventory until it is classified. Random draws
// and per-scope counters are NOT discoverable by introspection: a BIGINT column
// with a DEFAULT is indistinguishable from a counter, and a random id has no
// schema signature at all beyond a unique index. Their rows here are declared
// facts, and the inventory asserts the schema facts that carry each
// claim (default expression, ownership, storage type, unique index).
//
// Nothing here guards anything. It records what is guarded, and what is not.
//
// Every no-reuse claim carries its scope. Sequences and per-scope counters are
// lifetime-unique: a value that left the sequence, or a counter value already
// served, never comes back, deleted row or not. A random draw is unique only
// while the row that carries it exists, because the constraint refusing a
// repeat is that row's own primary key. Lifetime exclusion for the random-draw
// class (channels.id, polls.id) is therefore pending acceptance under the
// reservation stage; it is not delivered here and is not claimed here.
//
// The per-owner profile revision is classified below, now that the profile-photo
// schema is on this base. It is a per-scope counter with no sequence, so the
// sequence enumeration cannot discover it and its row is a declared fact, checked
// against the live column like every other non-sequence row. The next reservation
// stage inherits that row.
//
// The inert erasure-ledger persistence adds no allocator. Its outbox and its
// epoch/lineage markers carry no sequence, no column default and no counter: the
// record's epoch, stream and sequence arrive from the writer, and the contiguity a
// reader checks is exactly the contiguity the writer kept, so a schema-side
// allocator would erase the completeness signal the gap carries. The 16-byte
// operation_key, stream_id and lineage_id columns are opaque identities, not ids
// this schema hands out: no production path draws them in that slice, and the
// unique operation_key index is the interlock that refuses a repeated identity,
// not the constraint behind a draw. They become allocator rows, at the live-row
// scope a draw earns, when a writer starts drawing them. The sequence enumeration
// below is what proves the persistence slice left the allocator set
// untouched.

// allocatorClass is how far an allocated id travels.
type allocatorClass string

const (
	// classClientVisible: the id reaches a client in a wire field.
	classClientVisible allocatorClass = "client-visible"
	// classCrossReplica: the id is written by one replica and read by others,
	// so a collision or a reuse is not visible to the replica that made it.
	classCrossReplica allocatorClass = "internal-cross-replica"
	// classInternalOnly: one writer, the value never leaves the schema.
	classInternalOnly allocatorClass = "internal-only"
)

// allocatorKind is how the next value is produced.
type allocatorKind string

const (
	// kindSequenceDefault: the column default draws nextval at insert.
	kindSequenceDefault allocatorKind = "sequence-default"
	// kindSequenceDraw: Go calls nextval; the column carries no default.
	kindSequenceDraw allocatorKind = "sequence-draw"
	// kindRandomDraw: a crypto/rand draw refused by a unique index.
	kindRandomDraw allocatorKind = "random-draw"
	// kindScopeCounter: a per-scope column advanced by an UPDATE/INSERT.
	kindScopeCounter allocatorKind = "scope-counter"
)

// defaultKind is what the column default expression is, as introspected.
type defaultKind string

const (
	defaultNextval  defaultKind = "nextval"
	defaultConstant defaultKind = "constant"
	defaultNone     defaultKind = "none"
)

// Exhaustion guard states. guardNone is a recorded gap, not a pass.
const (
	guardSequenceCeiling = "sequence-ceiling" // cycle=false: Postgres refuses past MAXVALUE
	guardInt32Check      = "int32-check"      // Go refuses an id wider than the wire field
	guardCollisionRetry  = "collision-retry"  // bounded draw-and-retry against the unique index
	guardNone            = "none"             // nothing refuses an out-of-width value
)

// No-reuse scopes. scopeLiveRow is the weaker claim, and it is the whole of
// what a random draw offers today: it is not a partial form of
// scopeLifetime, and must not be read as one.
const (
	scopeLifetime = "lifetime"
	scopeLiveRow  = "live-row"
)

// allocatorFact is one classified allocator.
type allocatorFact struct {
	name     string // inventory key: "table.column"
	sequence string // qualified sequence name; empty for non-sequence allocators
	table    string
	column   string
	kind     allocatorKind

	owned         bool        // the sequence is OWNED BY this column
	columnDefault defaultKind // the column's default expression
	colType       string      // information_schema data_type of the column

	widthBits int            // width the value is served at
	class     allocatorClass // how far it travels
	guard     string         // what stops it running out
	wire      string         // the wire field that carries it, "" when internal
	noReuse   string         // the mechanism that keeps an issued id from coming back
}

// allocatorInventory is the classification at this base. Every sequence in the
// schema appears here; every row is checked against the live schema.
func allocatorInventory() []allocatorFact {
	return []allocatorFact{
		{
			name: "public.users.id", sequence: "public.users_id_seq",
			table: "users", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classClientVisible,
			guard: guardSequenceCeiling, wire: "tg.PeerUser.UserID (int64)",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.files.id", sequence: "public.files_id_seq",
			table: "files", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classClientVisible,
			guard: guardSequenceCeiling, wire: "tg.Document.ID (int64)",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.chats.id", sequence: "public.chats_id_seq",
			table: "chats", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classClientVisible,
			guard: guardSequenceCeiling, wire: "tg.PeerChat.ChatID (int64)",
			noReuse: "monotone non-cycling sequence",
		},
		{
			// The sequence survives (owned by the column) but the column default
			// was dropped, so an insert that omits id fails loudly instead of
			// drawing from it. ids are a crypto/rand draw since then.
			name: "public.channels.id", sequence: "public.channels_id_seq",
			table: "channels", column: "id",
			kind: kindRandomDraw, owned: true, columnDefault: defaultNone,
			colType: "bigint", widthBits: 64, class: classClientVisible,
			guard: guardCollisionRetry, wire: "tg.PeerChannel.ChannelID (int64)",
			noReuse: "uniform draw over [2^31, 10^12-2^31] refused by the primary key only while the row exists " +
				"(live-row); TestChannelsIDRequiresExplicitValue, TestCreateChannelRetriesOnIDCollision, " +
				"TestCreateChannelFailsAfterRepeatedCollisions; a deleted channel's id is back in the draw's range, " +
				"so lifetime exclusion pending acceptance",
		},
		{
			// int32 per the TL spec: the sequence is 64-bit, the column and the
			// wire field are 32-bit, and the Go allocation refuses the difference.
			name: "public.secret_chats.id", sequence: "public.secret_chats_id_seq",
			table: "secret_chats", column: "id",
			kind: kindSequenceDraw, owned: true, columnDefault: defaultNone,
			colType: "integer", widthBits: 32, class: classClientVisible,
			guard: guardInt32Check, wire: "tg.EncryptedChat.ID (int)",
			noReuse: "monotone non-cycling sequence; TestSecretChatIDsAreNeverReused",
		},
		{
			// No owner: the sequence belongs to no column, so it does not drop
			// with any table and is drawn explicitly by the fan-out insert.
			name: "public.messages.fanout_id", sequence: "public.message_fanout_seq",
			table: "messages", column: "fanout_id",
			kind: kindSequenceDraw, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 64, class: classCrossReplica,
			guard: guardSequenceCeiling, wire: "",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.phone_codes.id", sequence: "public.phone_codes_id_seq",
			table: "phone_codes", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classInternalOnly,
			guard: guardSequenceCeiling, wire: "",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.registration_invites.id", sequence: "public.registration_invites_id_seq",
			table: "registration_invites", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classInternalOnly,
			guard: guardSequenceCeiling, wire: "",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.chat_admin_events.id", sequence: "public.chat_admin_events_id_seq",
			table: "chat_admin_events", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classCrossReplica,
			guard: guardSequenceCeiling, wire: "",
			noReuse: "monotone non-cycling sequence",
		},
		{
			name: "public.language_catalog_publication_audit.id", sequence: "public.language_catalog_publication_audit_id_seq",
			table: "language_catalog_publication_audit", column: "id",
			kind: kindSequenceDefault, owned: true, columnDefault: defaultNextval,
			colType: "bigint", widthBits: 64, class: classInternalOnly,
			guard: guardSequenceCeiling, wire: "",
			noReuse: "monotone non-cycling sequence",
		},
		{
			// No sequence and no schema signature: a random draw whose only
			// discoverable property is the unique index that refuses a repeat.
			name: "public.polls.id", sequence: "",
			table: "polls", column: "id",
			kind: kindRandomDraw, owned: false, columnDefault: defaultNone,
			colType: "bigint", widthBits: 64, class: classClientVisible,
			guard: guardCollisionRetry, wire: "tg.Poll.ID, tg.UpdateMessagePoll.PollID (int64)",
			noReuse: "uniform draw over [1, MaxInt64] refused by the primary key only while the row exists " +
				"(live-row); TestPollCleanupKeepsAnotherPeersRetainedCopy and " +
				"TestConcurrentLastCopyDeletesSerializePollCleanup prove the id stays attached to a retained copy, " +
				"not that a deleted poll's id is never drawn again, so lifetime exclusion pending acceptance",
		},
		{
			// Per-owner message id and pts. Stored 64-bit, served 32-bit, and
			// nothing refuses the difference: the width refusal is a later stage.
			name: "public.update_state.next_local_id", sequence: "",
			table: "update_state", column: "next_local_id",
			kind: kindScopeCounter, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 32, class: classClientVisible,
			guard: guardNone, wire: "tg.Message.ID (int)",
			noReuse: "monotone per-owner counter, never decremented",
		},
		{
			name: "public.update_state.pts", sequence: "",
			table: "update_state", column: "pts",
			kind: kindScopeCounter, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 32, class: classClientVisible,
			guard: guardNone, wire: "tg.UpdateNewMessage.Pts, tg.UpdatesState.Pts (int)",
			noReuse: "monotone per-owner counter, never decremented",
		},
		{
			name: "public.channel_state.next_local_id", sequence: "",
			table: "channel_state", column: "next_local_id",
			kind: kindScopeCounter, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 32, class: classClientVisible,
			guard: guardNone, wire: "tg.Message.ID (int)",
			noReuse: "monotone per-channel counter, never decremented",
		},
		{
			name: "public.channel_state.pts", sequence: "",
			table: "channel_state", column: "pts",
			kind: kindScopeCounter, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 32, class: classClientVisible,
			guard: guardNone, wire: "tg.UpdatesChannelDifference.Pts (int)",
			noReuse: "monotone per-channel counter, never decremented",
		},
		{
			// The per-owner profile revision: one row per owner, advanced by that
			// owner's gallery writes. Live monotonicity is all this row claims: the
			// column is never decremented and the state row is not compacted, so a
			// revision already served does not come back inside the running database.
			// No writer exists for it in this schema yet, so no test coverage is claimed.
			//
			// Restore safety is a separate property and is NOT claimed here. A restore
			// from an older snapshot hands back that snapshot's lower counter, and a row
			// retained in the database being replaced cannot refuse it: retaining the
			// state row constrains the live system only, not a future snapshot of it.
			// Closing that gap is MAIN-1362's off-alpha reservation bounds plus the
			// gallery replay of MAIN-1443/1444, which is where a durable record of an
			// acknowledged photo clear can live at all.
			name: "public.profile_photo_state.mutation_revision", sequence: "",
			table: "profile_photo_state", column: "mutation_revision",
			kind: kindScopeCounter, owned: false, columnDefault: defaultConstant,
			colType: "bigint", widthBits: 64, class: classInternalOnly,
			guard: guardNone, wire: "",
			noReuse: "monotone per-owner revision, never decremented, so a revision already served does not come back inside the running database; restore safety is a separate property and is not claimed by this row",
		},
	}
}

// wireAnchor is a wire field whose Go type fixes the served width: TL int and
// int32 serialize as one 4-byte word, TL long and int64 as one 8-byte word.
type wireAnchor struct {
	allocator string
	value     any
	field     string
}

// wireAnchors pair every client-visible row with the gotd field that carries it.
func wireAnchors() []wireAnchor {
	return []wireAnchor{
		{allocator: "public.users.id", value: tg.PeerUser{}, field: "UserID"},
		{allocator: "public.files.id", value: tg.Document{}, field: "ID"},
		{allocator: "public.chats.id", value: tg.PeerChat{}, field: "ChatID"},
		{allocator: "public.channels.id", value: tg.PeerChannel{}, field: "ChannelID"},
		{allocator: "public.secret_chats.id", value: tg.EncryptedChat{}, field: "ID"},
		{allocator: "public.polls.id", value: tg.Poll{}, field: "ID"},
		{allocator: "public.update_state.next_local_id", value: tg.Message{}, field: "ID"},
		{allocator: "public.channel_state.next_local_id", value: tg.Message{}, field: "ID"},
		{allocator: "public.update_state.pts", value: tg.UpdateNewMessage{}, field: "Pts"},
		{allocator: "public.channel_state.pts", value: tg.UpdatesChannelDifference{}, field: "Pts"},
	}
}

// wireWidth returns the serialized width of a gotd struct field in bits.
func wireWidth(t *testing.T, value any, field string) int {
	t.Helper()
	f, ok := reflect.TypeOf(value).FieldByName(field)
	if !ok {
		t.Fatalf("%T has no field %s", value, field)
	}
	switch f.Type.Kind() {
	case reflect.Int, reflect.Int32:
		return 32
	case reflect.Int64:
		return 64
	default:
		t.Fatalf("%T.%s is %s, want int, int32 or int64", value, field, f.Type)
		return 0
	}
}

// storageWidth maps an information_schema data_type to the bits it stores.
func storageWidth(colType string) (int, bool) {
	switch colType {
	case "integer":
		return 32, true
	case "bigint":
		return 64, true
	default:
		return 0, false
	}
}

// sequenceRow is one enumerated sequence with its owner.
type sequenceRow struct {
	name     string
	owner    string // "table.column" the sequence is OWNED BY, "" when unowned
	dataType string
	cycles   bool
}

// columnRow is one introspected column.
type columnRow struct {
	dataType string
	def      string // column_default, "" when there is none
}

// inventoryState is everything the schema says about ids it hands out.
type inventoryState struct {
	sequences []sequenceRow
	columns   map[string]columnRow // key "public.table.column"
	unique    map[string]bool      // columns covered by a single-column unique index
}

// readInventoryState introspects the disposable database the store is open on.
func readInventoryState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) inventoryState {
	t.Helper()
	state := inventoryState{
		columns: map[string]columnRow{},
		unique:  map[string]bool{},
	}

	seqSQL := `
		SELECT n.nspname || '.' || s.relname,
		       COALESCE(ct.relname, '') || CASE WHEN a.attname IS NULL THEN '' ELSE '.' || a.attname END,
		       format_type(p.seqtypid, NULL),
		       COALESCE(p.seqcycle, false)
		  FROM pg_class s
		  JOIN pg_namespace n ON n.oid = s.relnamespace
		  LEFT JOIN pg_sequence p ON p.seqrelid = s.oid
		  LEFT JOIN pg_depend d
		         ON d.classid = 'pg_class'::regclass
		        AND d.refclassid = 'pg_class'::regclass
		        AND d.objid = s.oid
		        AND d.deptype = 'a'
		        AND d.refobjsubid > 0
		  LEFT JOIN pg_class ct ON ct.oid = d.refobjid
		  LEFT JOIN pg_attribute a ON a.attrelid = ct.oid AND a.attnum = d.refobjsubid
		 WHERE s.relkind = 'S'
		   AND n.nspname = 'public'
		 ORDER BY 1`
	rows, err := pool.Query(ctx, seqSQL)
	if err != nil {
		t.Fatalf("list sequences: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name, owner, dataType string
			cycles                bool
		)
		if err := rows.Scan(&name, &owner, &dataType, &cycles); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		if owner == "." || owner == "" {
			owner = ""
		}
		state.sequences = append(state.sequences, sequenceRow{name: name, owner: owner, dataType: dataType, cycles: cycles})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sequences: %v", err)
	}

	colSQL := `
		SELECT table_name, column_name, data_type, COALESCE(column_default, '')
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = ANY($1)
		 ORDER BY table_name, column_name`
	tables := make([]string, 0, len(allocatorInventory()))
	for _, f := range allocatorInventory() {
		tables = append(tables, f.table)
	}
	colRows, err := pool.Query(ctx, colSQL, tables)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var table, column, dataType, def string
		if err := colRows.Scan(&table, &column, &dataType, &def); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		state.columns["public."+table+"."+column] = columnRow{dataType: dataType, def: def}
	}
	if err := colRows.Err(); err != nil {
		t.Fatalf("read columns: %v", err)
	}

	uniqueSQL := `
		SELECT DISTINCT n.nspname || '.' || t.relname || '.' || a.attname
		  FROM pg_index i
		  JOIN pg_class t ON t.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = i.indkey[0]
		 WHERE n.nspname = 'public'
		   AND i.indisunique
		   AND i.indnkeyatts = 1`
	uniqueRows, err := pool.Query(ctx, uniqueSQL)
	if err != nil {
		t.Fatalf("list unique indexes: %v", err)
	}
	defer uniqueRows.Close()
	for uniqueRows.Next() {
		var name string
		if err := uniqueRows.Scan(&name); err != nil {
			t.Fatalf("scan unique index: %v", err)
		}
		state.unique[name] = true
	}
	if err := uniqueRows.Err(); err != nil {
		t.Fatalf("read unique indexes: %v", err)
	}
	return state
}

// nextvalRE pulls the sequence a column default draws, if any.
var nextvalRE = regexp.MustCompile(`nextval\('([^']*)'`)

// drawnSequence returns the sequence a column_default draws, with the public
// schema prefix dropped (regclass renders unqualified there), "" when the
// default draws no sequence.
func drawnSequence(def string) string {
	m := nextvalRE.FindStringSubmatch(def)
	if m == nil {
		return ""
	}
	return strings.TrimPrefix(m[1], "public.")
}

// bareName drops the public schema prefix from a qualified object name.
func bareName(qualified string) string {
	return strings.TrimPrefix(qualified, "public.")
}

// noReuseScope returns how long a row's no-reuse mechanism holds. A random
// draw is unique only while the row carrying it exists: the constraint that
// refuses a repeat is that row's own primary key, and it goes away with the
// row. A sequence value and a served counter value never come back.
func noReuseScope(f allocatorFact) string {
	if f.kind == kindRandomDraw {
		return scopeLiveRow
	}
	return scopeLifetime
}

// inventoryProblems classifies the schema against the inventory. Every problem
// names the offending object, so an unclassified addition is reported, not
// skipped. It is pure: the mutation case feeds it a hand-modified state.
func inventoryProblems(state inventoryState, inventory []allocatorFact) []string {
	var problems []string
	bySequence := make(map[string]allocatorFact, len(inventory))
	for _, f := range inventory {
		if f.sequence != "" {
			bySequence[f.sequence] = f
		}
	}

	for _, seq := range state.sequences {
		f, ok := bySequence[seq.name]
		if !ok {
			problems = append(problems, fmt.Sprintf("unclassified sequence %s: add a classified row for it", seq.name))
			continue
		}
		if seq.dataType != "" {
			bits, known := storageWidth(seq.dataType)
			if !known {
				problems = append(problems, fmt.Sprintf("sequence %s has unexpected data type %q", seq.name, seq.dataType))
			} else if bits < f.widthBits {
				problems = append(problems, fmt.Sprintf("sequence %s stores %d bits, below the %d-bit served width of %s",
					seq.name, bits, f.widthBits, f.name))
			}
		}
		if seq.cycles {
			problems = append(problems, fmt.Sprintf("sequence %s cycles, so it can hand back an issued id", seq.name))
		}
		owner := f.table + "." + f.column
		if f.owned && seq.owner != owner {
			problems = append(problems, fmt.Sprintf("sequence %s is owned by %q, inventory says %q", seq.name, seq.owner, owner))
		}
		if !f.owned && seq.owner != "" {
			problems = append(problems, fmt.Sprintf("sequence %s is owned by %q, inventory says it has no owner", seq.name, seq.owner))
		}
	}

	for _, f := range inventory {
		if f.sequence != "" {
			var found bool
			for _, seq := range state.sequences {
				if seq.name == f.sequence {
					found = true
					break
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("%s classifies sequence %s, which the schema does not have", f.name, f.sequence))
			}
		} else {
			for _, seq := range state.sequences {
				if seq.owner == f.table+"."+f.column {
					problems = append(problems, fmt.Sprintf("%s is classified as a non-sequence allocator, but %s owns the column", f.name, seq.name))
				}
			}
		}

		if f.widthBits != 32 && f.widthBits != 64 {
			problems = append(problems, fmt.Sprintf("%s declares served width %d, want 32 or 64", f.name, f.widthBits))
		}
		if f.class != classClientVisible && f.class != classCrossReplica && f.class != classInternalOnly {
			problems = append(problems, fmt.Sprintf("%s declares class %q, which is not one of the three", f.name, f.class))
		}
		switch f.guard {
		case guardSequenceCeiling, guardInt32Check, guardCollisionRetry, guardNone:
		default:
			problems = append(problems, fmt.Sprintf("%s declares guard %q, which is not one of the four", f.name, f.guard))
		}
		if f.noReuse == "" {
			problems = append(problems, f.name+" records no durable no-reuse mechanism")
		}
		if noReuseScope(f) == scopeLiveRow {
			for _, want := range []string{scopeLiveRow, "pending acceptance"} {
				if !strings.Contains(f.noReuse, want) {
					problems = append(problems, fmt.Sprintf("%s is a random draw whose no-reuse text does not record %q", f.name, want))
				}
			}
		}
		if f.kind == kindRandomDraw && !state.unique[f.name] {
			problems = append(problems, f.name+" is a random draw with no single-column unique index to refuse a repeat")
		}

		col, ok := state.columns[f.name]
		if !ok {
			problems = append(problems, f.name+" is classified, but the schema has no such column")
			continue
		}
		if col.dataType != f.colType {
			problems = append(problems, fmt.Sprintf("%s stores %q, inventory says %q", f.name, col.dataType, f.colType))
		}
		storage, known := storageWidth(col.dataType)
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s has unexpected storage type %q", f.name, col.dataType))
		case storage < f.widthBits:
			problems = append(problems, fmt.Sprintf("%s stores %d bits, below the %d-bit served width", f.name, storage, f.widthBits))
		case storage > f.widthBits && f.guard != guardNone:
			problems = append(problems, fmt.Sprintf("%s stores %d bits and is served at %d, so %s claims a guard the width gap needs",
				f.name, storage, f.widthBits, f.guard))
		}

		switch f.columnDefault {
		case defaultNextval:
			drawn := drawnSequence(col.def)
			if drawn == "" {
				problems = append(problems, fmt.Sprintf("%s default is %q, inventory says it draws a sequence", f.name, col.def))
			} else if drawn != bareName(f.sequence) {
				problems = append(problems, fmt.Sprintf("%s default draws sequence %q, inventory says %s", f.name, drawn, f.sequence))
			}
		case defaultConstant:
			if col.def == "" || drawnSequence(col.def) != "" {
				problems = append(problems, fmt.Sprintf("%s default is %q, inventory says it is a constant", f.name, col.def))
			}
		case defaultNone:
			if col.def != "" {
				problems = append(problems, fmt.Sprintf("%s default is %q, inventory says it has none", f.name, col.def))
			}
		}
	}
	return problems
}

// TestAllocatorInventoryClassifiesEverySequence enumerates the schema's
// sequences and classifies exactly the ten this base ships.
func TestAllocatorInventoryClassifiesEverySequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	state := readInventoryState(t, ctx, store.StorePool(s))
	inventory := allocatorInventory()

	if problems := inventoryProblems(state, inventory); len(problems) > 0 {
		t.Fatalf("inventory:\n  %s", strings.Join(problems, "\n  "))
	}

	want := make([]string, 0, len(inventory))
	for _, f := range inventory {
		if f.sequence != "" {
			want = append(want, f.sequence)
		}
	}
	sort.Strings(want)
	got := make([]string, 0, len(state.sequences))
	for _, seq := range state.sequences {
		got = append(got, seq.name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("schema sequences [%d]: %s\ninventory [%d]: %s",
			len(got), strings.Join(got, ","), len(want), strings.Join(want, ","))
	}
}

// TestAllocatorInventoryRejectsUnclassifiedSequence is the mutation case: a
// test-only sequence in the disposable schema must fail validation and be named.
func TestAllocatorInventoryRejectsUnclassifiedSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	pool := store.StorePool(s)
	inventory := allocatorInventory()

	if problems := inventoryProblems(readInventoryState(t, ctx, pool), inventory); len(problems) > 0 {
		t.Fatalf("unmutated inventory:\n  %s", strings.Join(problems, "\n  "))
	}

	const probe = "public.inventory_probe_unclassified_seq"
	if _, err := pool.Exec(ctx, "CREATE SEQUENCE "+probe); err != nil {
		t.Fatalf("create probe sequence: %v", err)
	}
	problems := inventoryProblems(readInventoryState(t, ctx, pool), inventory)
	if len(problems) == 0 {
		t.Fatalf("inventory passed with the unclassified sequence %s present", probe)
	}
	var named bool
	for _, p := range problems {
		if strings.Contains(p, probe) && strings.Contains(p, "unclassified") {
			named = true
		}
	}
	if !named {
		t.Fatalf("inventory problems do not name %s:\n  %s", probe, strings.Join(problems, "\n  "))
	}

	if _, err := pool.Exec(ctx, "DROP SEQUENCE "+probe); err != nil {
		t.Fatalf("drop probe sequence: %v", err)
	}
	if problems := inventoryProblems(readInventoryState(t, ctx, pool), inventory); len(problems) > 0 {
		t.Fatalf("inventory after dropping the probe:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestAllocatorInventoryOwnershipAndDefaults checks the owned/default
// state of every row against the live schema, including the two that look
// simplifiable: channels.id keeps an owned sequence it no longer draws from,
// and message_fanout_seq has no owning column at all.
func TestAllocatorInventoryOwnershipAndDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	state := readInventoryState(t, ctx, store.StorePool(s))
	inventory := allocatorInventory()

	ownerBySequence := map[string]string{}
	for _, seq := range state.sequences {
		ownerBySequence[seq.name] = seq.owner
	}

	for _, f := range inventory {
		col, ok := state.columns[f.name]
		if !ok {
			t.Errorf("%s: column absent from the schema", f.name)
			continue
		}
		switch f.columnDefault {
		case defaultNextval:
			if drawn := drawnSequence(col.def); drawn != bareName(f.sequence) {
				t.Errorf("%s default %q draws %q, want %s", f.name, col.def, drawn, f.sequence)
			}
		case defaultConstant:
			if col.def == "" || drawnSequence(col.def) != "" {
				t.Errorf("%s default %q is not a constant", f.name, col.def)
			}
		case defaultNone:
			if col.def != "" {
				t.Errorf("%s default %q, want none", f.name, col.def)
			}
		}
		if f.sequence == "" {
			continue
		}
		owner, present := ownerBySequence[f.sequence]
		if !present {
			t.Errorf("%s: sequence %s absent from the schema", f.name, f.sequence)
			continue
		}
		want := f.table + "." + f.column
		if f.owned && owner != want {
			t.Errorf("%s: sequence %s owned by %q, want %q", f.name, f.sequence, owner, want)
		}
		if !f.owned && owner != "" {
			t.Errorf("%s: sequence %s owned by %q, want no owner", f.name, f.sequence, owner)
		}
	}

	channels := state.columns["public.channels.id"]
	if channels.def != "" {
		t.Errorf("channels.id default %q, want no default: ids are random draws", channels.def)
	}
	if owner := ownerBySequence["public.channels_id_seq"]; owner != "channels.id" {
		t.Errorf("channels_id_seq owned by %q, want the surviving channels.id ownership", owner)
	}
	if owner, ok := ownerBySequence["public.message_fanout_seq"]; !ok || owner != "" {
		t.Errorf("message_fanout_seq owner %q present=%v, want an unowned sequence", owner, ok)
	}
}

// TestAllocatorInventoryServedWidths asserts the served width of every row:
// 32 bits for secret-chat ids and the scoped local_id/pts counters, 64 for the
// rest. Client-visible widths are measured at the gotd wire field, and the wire
// word sizes are measured against gotd's own encoder.
func TestAllocatorInventoryServedWidths(t *testing.T) {
	t.Parallel()

	// The word sizes TL integer types occupy, from the encoder that writes them.
	intWord := new(bin.Buffer)
	intWord.PutInt(math.MaxInt32)
	longWord := new(bin.Buffer)
	longWord.PutLong(math.MaxInt64)
	if len(intWord.Buf) != 4 {
		t.Errorf("TL int encodes to %d bytes, want 4", len(intWord.Buf))
	}
	if len(longWord.Buf) != 8 {
		t.Errorf("TL long encodes to %d bytes, want 8", len(longWord.Buf))
	}

	inventory := allocatorInventory()
	byName := make(map[string]allocatorFact, len(inventory))
	for _, f := range inventory {
		byName[f.name] = f
	}

	anchored := map[string]bool{}
	for _, a := range wireAnchors() {
		f, ok := byName[a.allocator]
		if !ok {
			t.Fatalf("wire anchor for unclassified allocator %s", a.allocator)
		}
		if bits := wireWidth(t, a.value, a.field); bits != f.widthBits {
			t.Errorf("%s served width %d bits, wire field %T.%s is %d",
				a.allocator, f.widthBits, a.value, a.field, bits)
		}
		anchored[a.allocator] = true
	}

	var int32s, int64s []string
	for _, f := range inventory {
		if f.class == classClientVisible && !anchored[f.name] {
			t.Errorf("%s is client-visible with no wire anchor", f.name)
		}
		if f.class != classClientVisible && anchored[f.name] {
			t.Errorf("%s has a wire anchor but is classified %s", f.name, f.class)
		}
		if f.widthBits == 32 {
			int32s = append(int32s, f.name)
		} else {
			int64s = append(int64s, f.name)
		}
	}

	want32 := []string{
		"public.secret_chats.id",
		"public.update_state.next_local_id",
		"public.update_state.pts",
		"public.channel_state.next_local_id",
		"public.channel_state.pts",
	}
	sortedWant := append([]string(nil), want32...)
	sort.Strings(sortedWant)
	sort.Strings(int32s)
	if strings.Join(int32s, ",") != strings.Join(sortedWant, ",") {
		t.Errorf("int32-served set = %s, want %s", strings.Join(int32s, ","), strings.Join(sortedWant, ","))
	}
	if len(int64s) != len(inventory)-len(want32) {
		t.Errorf("int64-served count = %d, want %d: %s", len(int64s), len(inventory)-len(want32), strings.Join(int64s, ","))
	}

	// The width gap that the recorded guardNone rows carry: stored 64,
	// served 32. Asserted so the gap is a fact in the test, not a comment.
	ctx := context.Background()
	s := open(t)
	state := readInventoryState(t, ctx, store.StorePool(s))
	for _, name := range []string{
		"public.update_state.next_local_id",
		"public.update_state.pts",
		"public.channel_state.next_local_id",
		"public.channel_state.pts",
	} {
		f := byName[name]
		if f.widthBits != 32 || f.guard != guardNone {
			t.Errorf("%s: width %d guard %q, want a 32-bit counter with no guard", name, f.widthBits, f.guard)
		}
		if col := state.columns[name]; col.dataType != "bigint" {
			t.Errorf("%s stores %q, want bigint above the 32-bit served width", name, col.dataType)
		}
	}
	if secret := byName["public.secret_chats.id"]; secret.widthBits != 32 || secret.guard != guardInt32Check {
		t.Errorf("secret_chats.id: width %d guard %q, want a 32-bit id behind an explicit check", secret.widthBits, secret.guard)
	}
	if col := state.columns["public.secret_chats.id"]; col.dataType != "integer" {
		t.Errorf("secret_chats.id stores %q, want integer", col.dataType)
	}
	secretID, ok := reflect.TypeFor[store.SecretChat]().FieldByName("ID")
	if !ok || secretID.Type.Kind() != reflect.Int32 {
		t.Errorf("store.SecretChat.ID is %s, want int32", secretID.Type)
	}
}

// TestAllocatorInventoryExposureClasses separates the three travel classes and
// keeps message_fanout and chat_admin_events named in cross-replica coverage.
func TestAllocatorInventoryExposureClasses(t *testing.T) {
	t.Parallel()
	inventory := allocatorInventory()
	classes := map[allocatorClass][]string{
		classClientVisible: {},
		classCrossReplica:  {},
		classInternalOnly:  {},
	}
	for _, f := range inventory {
		classes[f.class] = append(classes[f.class], f.name)
	}

	want := map[allocatorClass][]string{
		classClientVisible: {
			"public.users.id", "public.files.id", "public.chats.id", "public.channels.id",
			"public.secret_chats.id", "public.polls.id",
			"public.update_state.next_local_id", "public.update_state.pts",
			"public.channel_state.next_local_id", "public.channel_state.pts",
		},
		classCrossReplica: {
			"public.messages.fanout_id", "public.chat_admin_events.id",
		},
		classInternalOnly: {
			"public.phone_codes.id", "public.registration_invites.id",
			"public.language_catalog_publication_audit.id",
			"public.profile_photo_state.mutation_revision",
		},
	}
	for class, names := range want {
		got := append([]string(nil), classes[class]...)
		sort.Strings(got)
		sort.Strings(names)
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("%s class = %s, want %s", class, strings.Join(got, ","), strings.Join(names, ","))
		}
	}

	total := 0
	for _, names := range classes {
		total += len(names)
	}
	if total != len(inventory) {
		t.Errorf("classes cover %d allocators, want %d (the classes must be disjoint)", total, len(inventory))
	}
}

// TestAllocatorInventoryReservationModes keeps the recovery vocabulary aligned
// with every allocator in the schema. Global sequences use absolute maxima,
// scoped counters use per-stream component sums, and random draws remain
// unclassified until their separate lifetime-exclusion contract lands.
func TestAllocatorInventoryReservationModes(t *testing.T) {
	t.Parallel()
	for _, f := range allocatorInventory() {
		var name string
		switch f.kind {
		case kindRandomDraw:
			if f.sequence != "" {
				name = bareName(f.sequence)
			} else {
				name = strings.ReplaceAll(f.table+"_"+f.column, ".", "_")
			}
			allocator, err := erasureledger.ValidateAllocator(name)
			if err != nil {
				t.Fatalf("ValidateAllocator(%q): %v", name, err)
			}
			if _, ok := erasureledger.ClassifyAllocator(allocator); ok {
				t.Errorf("random draw %s was classified for scalar/component reservation", f.name)
			}
		case kindScopeCounter:
			name = strings.ReplaceAll(f.table+"_"+f.column, ".", "_")
			allocator, err := erasureledger.ValidateAllocator(name)
			if err != nil {
				t.Fatalf("ValidateAllocator(%q): %v", name, err)
			}
			mode, ok := erasureledger.ClassifyAllocator(allocator)
			if !ok || mode != erasureledger.AllocatorComponentSum {
				t.Errorf("%s classification = %v, %v; want component-sum", f.name, mode, ok)
			}
		default:
			name = bareName(f.sequence)
			allocator, err := erasureledger.ValidateAllocator(name)
			if err != nil {
				t.Fatalf("ValidateAllocator(%q): %v", name, err)
			}
			mode, ok := erasureledger.ClassifyAllocator(allocator)
			if !ok || mode != erasureledger.AllocatorAbsoluteMax {
				t.Errorf("%s classification = %v, %v; want absolute-max", f.name, mode, ok)
			}
		}
	}
}

// TestAllocatorInventoryRecordsUnguardedClasses records which classes
// lack an exhaustion guard. It does not add one: a width refusal for local_id
// and pts is a later stage of the sequence this inventory feeds.
func TestAllocatorInventoryRecordsUnguardedClasses(t *testing.T) {
	t.Parallel()
	inventory := allocatorInventory()

	var unguarded, guarded []string
	for _, f := range inventory {
		if f.guard == guardNone {
			unguarded = append(unguarded, f.name)
			continue
		}
		guarded = append(guarded, f.name)
	}
	sort.Strings(unguarded)

	want := []string{
		"public.channel_state.next_local_id",
		"public.channel_state.pts",
		"public.profile_photo_state.mutation_revision",
		"public.update_state.next_local_id",
		"public.update_state.pts",
	}
	if strings.Join(unguarded, ",") != strings.Join(want, ",") {
		t.Errorf("allocators with no exhaustion guard = %s, want %s", strings.Join(unguarded, ","), strings.Join(want, ","))
	}
	t.Logf("guarded: %d of %d allocators: %s", len(guarded), len(inventory), strings.Join(guarded, ","))

	for _, f := range inventory {
		switch f.kind {
		case kindRandomDraw:
			if f.guard != guardCollisionRetry {
				t.Errorf("%s is a random draw with guard %q, want a bounded collision retry", f.name, f.guard)
			}
		case kindScopeCounter:
			if f.guard != guardNone {
				t.Errorf("%s is a per-scope counter with guard %q, want none recorded", f.name, f.guard)
			}
		default:
			if f.guard != guardSequenceCeiling && f.guard != guardInt32Check {
				t.Errorf("%s is sequence-backed with guard %q, want a sequence ceiling or an int32 check", f.name, f.guard)
			}
		}
	}
}

// TestAllocatorInventoryDurableNoReuse records the scope of every no-reuse
// claim and checks the mechanism each rests on: a non-cycling sequence, a
// counter that is never decremented, or a unique index behind a random draw.
// Random draws are live-row unique only, so the inventory names them as
// lifetime exclusion pending acceptance. This test records that limit; it does
// not close it.
func TestAllocatorInventoryDurableNoReuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	state := readInventoryState(t, ctx, store.StorePool(s))
	inventory := allocatorInventory()

	for _, seq := range state.sequences {
		if seq.cycles {
			t.Errorf("sequence %s cycles: an issued id can come back", seq.name)
		}
	}

	byName := make(map[string]allocatorFact, len(inventory))
	for _, f := range inventory {
		byName[f.name] = f
	}

	var pending []string
	for _, f := range inventory {
		switch noReuseScope(f) {
		case scopeLiveRow:
			pending = append(pending, f.name)
			if !state.unique[f.name] {
				t.Errorf("%s has no single-column unique index for its %s draw to lean on", f.name, f.kind)
			}
			for _, want := range []string{scopeLiveRow, "pending acceptance"} {
				if !strings.Contains(f.noReuse, want) {
					t.Errorf("%s is a %s draw: its no-reuse text must record %q, got %q", f.name, f.kind, want, f.noReuse)
				}
			}
		case scopeLifetime:
			if strings.Contains(f.noReuse, "pending acceptance") {
				t.Errorf("%s is lifetime-unique, so its no-reuse text must not record a pending exclusion", f.name)
			}
		}
	}
	sort.Strings(pending)
	wantPending := []string{"public.channels.id", "public.polls.id"}
	if strings.Join(pending, ",") != strings.Join(wantPending, ",") {
		t.Errorf("allocators with lifetime exclusion pending = %s, want %s",
			strings.Join(pending, ","), strings.Join(wantPending, ","))
	}
	t.Logf("lifetime exclusion pending acceptance: %s", strings.Join(pending, ", "))

	// Coverage is checked by name, so it cannot be renamed away silently. The
	// secret-chat test proves the sequence path hands nothing back, across a
	// discard: lifetime. The poll and channel tests prove the live-row path:
	// an id stays attached while a copy is retained, and a collision redraws.
	// None of them proves that a deleted row's id is never drawn again, and
	// none is recorded as if it did.
	coverage := map[string][]string{
		"public.secret_chats.id": {"TestSecretChatIDsAreNeverReused"},
		"public.polls.id": {
			"TestPollCleanupKeepsAnotherPeersRetainedCopy",
			"TestConcurrentLastCopyDeletesSerializePollCleanup",
		},
		"public.channels.id": {
			"TestChannelsIDRequiresExplicitValue",
			"TestCreateChannelRetriesOnIDCollision",
			"TestCreateChannelFailsAfterRepeatedCollisions",
		},
	}
	testNames := localTestNames(t)
	secret := byName["public.secret_chats.id"]
	if noReuseScope(secret) != scopeLifetime || !strings.Contains(secret.noReuse, "sequence") {
		t.Errorf("secret_chats.id claims coverage as scope %s off %q, want a lifetime claim naming the sequence",
			noReuseScope(secret), secret.noReuse)
	}
	for name, tests := range coverage {
		f := byName[name]
		for _, test := range tests {
			if !testNames[test] {
				t.Errorf("%s claims no-reuse coverage by %s, which this package no longer defines", name, test)
			}
			if !strings.Contains(f.noReuse, test) {
				t.Errorf("%s covers %s, which the inventory does not name", name, test)
			}
		}
	}
}

// localTestNames returns every Test function defined in this package.
func localTestNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names[fn.Name.Name] = true
			}
		}
	}
	return names
}
