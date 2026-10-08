package erasureledger_test

// The two storage arms under the synthetic provider seam. The capability,
// sequencing and evidence logic lives in the seam, so an arm's job is to be a
// storage medium: the memory arm answers from maps, and the disposable-directory
// arm answers from files in a directory the test deletes. The directory arm is
// the one that makes the create-once condition a filesystem fact: the
// refusal comes from the exclusive create, not from a map probe.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// newMemoryStore opens the in-memory arm.
func newMemoryStore(t *testing.T) store {
	t.Helper()
	return &memoryStore{
		objects:    map[string]*object{},
		quarantine: map[string]*object{},
		markers:    map[string]*object{},
		streams:    map[streamKey]streamRec{},
		keeps:      map[string]string{},
	}
}

// memoryStore is the map arm. Objects are copied in and out, so a stored
// object is the medium's own bytes and not a caller's alias.
type memoryStore struct {
	objects    map[string]*object
	quarantine map[string]*object
	markers    map[string]*object
	streams    map[streamKey]streamRec
	keeps      map[string]string
	clock      int64
	arrivals   int64
}

func (m *memoryStore) providerState() (int64, int64) { return m.clock, m.arrivals }

func (m *memoryStore) saveProviderState(clock, arrivals int64) {
	m.clock, m.arrivals = clock, arrivals
}

func (m *memoryStore) streamState(sk streamKey) (streamRec, bool) {
	rec, ok := m.streams[sk]
	return rec, ok
}

func (m *memoryStore) saveStreamState(sk streamKey, rec streamRec) { m.streams[sk] = rec }

func (m *memoryStore) getObject(name string) *object {
	obj, ok := m.objects[name]
	if !ok {
		return nil
	}
	return cloneObject(obj)
}

func (m *memoryStore) saveObjectOnce(obj *object) error {
	if _, taken := m.objects[obj.name]; taken {
		return fmt.Errorf("%w: %s", errObjectExists, obj.name)
	}
	m.objects[obj.name] = cloneObject(obj)
	return nil
}

func (m *memoryStore) saveObject(obj *object) { m.objects[obj.name] = cloneObject(obj) }

func (m *memoryStore) deleteObject(name string) { delete(m.objects, name) }

// page lists the confirmed objects only: an unconfirmed write is not in the
// ledger, so a listing cannot show it, exactly as an uncompleted multipart
// upload is not a listed object.
func (m *memoryStore) page(after string, limit int) ([]*object, string) {
	names := slices.Sorted(maps.Keys(m.objects))
	out := make([]*object, 0, limit)
	last := ""
	for _, name := range names {
		obj := m.objects[name]
		if obj.state != stateConfirmed {
			continue
		}
		last = name
		if name <= after || len(out) == limit {
			continue
		}
		out = append(out, cloneObject(obj))
	}
	return out, last
}

func (m *memoryStore) quarantineObject(obj *object) { m.quarantine[obj.name] = cloneObject(obj) }

func (m *memoryStore) quarantinedObjects() []*object {
	names := slices.Sorted(maps.Keys(m.quarantine))
	out := make([]*object, 0, len(names))
	for _, name := range names {
		out = append(out, cloneObject(m.quarantine[name]))
	}
	return out
}

func (m *memoryStore) writeMarker(name string, obj *object) bool {
	if _, taken := m.markers[name]; taken {
		return false
	}
	m.markers[name] = cloneObject(obj)
	return true
}

func (m *memoryStore) readMarker(name string) *object {
	obj, ok := m.markers[name]
	if !ok {
		return nil
	}
	return cloneObject(obj)
}

func (m *memoryStore) keepMarker(class string) string { return m.keeps[class] }

func (m *memoryStore) saveKeepMarker(class, name string) { m.keeps[class] = name }

// cloneObject copies an object, body included, so the arm owns its bytes.
func cloneObject(obj *object) *object {
	out := *obj
	out.body = slices.Clone(obj.body)
	return &out
}

// openDirStore opens the disposable-directory arm under a test-owned
// directory. It is local and throwaway: no volume, no mount, no image, no
// credential, and the test removes the directory.
func openDirStore(t *testing.T) store {
	t.Helper()
	root := t.TempDir()
	d := &dirStore{t: t, root: root}
	for _, sub := range []string{"objects", "quarantine", "markers", "streams", "keeps"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatalf("synthledger: mkdir %s: %v", sub, err)
		}
	}
	return d
}

// dirStore is the directory arm. Every read goes to disk, so a value the seam
// reads back is a value the medium returned, and a create-once refusal is the
// filesystem's exclusive-create refusal.
type dirStore struct {
	t    *testing.T
	root string
}

// objectFile is the on-disk shape of one object. Its identity fields are the
// opaque values in hex, so a file name and the key, stream, and arrival fields
// carry nothing derived from a record's contents. Body is the encoded record
// itself, hex-encoded: the medium stores exactly the bytes the codec accepted
// for the record, and adds no per-record data of its own.
type objectFile struct {
	Key        string `json:"key"`
	Body       string `json:"body"`
	Epoch      int64  `json:"epoch"`
	Stream     string `json:"stream"`
	Seq        int64  `json:"seq"`
	ClockNanos int64  `json:"clock_nanos"`
	ArrivalSeq int64  `json:"arrival_seq"`
	Checksum   uint32 `json:"checksum"`
	Confirmed  bool   `json:"confirmed"`
	Keep       string `json:"keep,omitempty"`
}

// providerFile is the on-disk shape of the provider's own counters.
type providerFile struct {
	ClockNanos int64 `json:"clock_nanos"`
	Arrivals   int64 `json:"arrivals"`
}

// streamFile is the on-disk shape of one stream's state.
type streamFile struct {
	PendingSeq   int64 `json:"pending_seq"`
	ConfirmedSeq int64 `json:"confirmed_seq"`
	ArrivalCount int64 `json:"arrival_count"`
	Pruned       int64 `json:"pruned_through"`
	Fenced       bool  `json:"fenced"`
	FencedSeq    int64 `json:"fenced_at"`
}

// keepFile is the on-disk shape of one keep-marker pointer.
type keepFile struct {
	Name string `json:"name"`
}

func (d *dirStore) path(parts ...string) string {
	return filepath.Join(append([]string{d.root}, parts...)...)
}

func (d *dirStore) providerState() (int64, int64) {
	raw, err := os.ReadFile(d.path("provider.json"))
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		d.t.Fatalf("synthledger: provider state read: %v", err)
	}
	var pf providerFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		d.t.Fatalf("synthledger: provider state does not parse: %v", err)
	}
	return pf.ClockNanos, pf.Arrivals
}

func (d *dirStore) saveProviderState(clock, arrivals int64) {
	d.write(d.path("provider.json"), d.json(providerFile{ClockNanos: clock, Arrivals: arrivals}))
}

func (d *dirStore) streamState(sk streamKey) (streamRec, bool) {
	raw, err := os.ReadFile(d.path("streams", streamName(sk)))
	if os.IsNotExist(err) {
		return streamRec{}, false
	}
	if err != nil {
		d.t.Fatalf("synthledger: stream state read: %v", err)
	}
	var sf streamFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		d.t.Fatalf("synthledger: stream state does not parse: %v", err)
	}
	return streamRec{
		pendingSeq: sf.PendingSeq, confirmedSeq: sf.ConfirmedSeq,
		arrivalCount: sf.ArrivalCount, pruned: sf.Pruned,
		fenced: sf.Fenced, fencedSeq: sf.FencedSeq,
	}, true
}

func (d *dirStore) saveStreamState(sk streamKey, rec streamRec) {
	d.write(d.path("streams", streamName(sk)), d.json(streamFile{
		PendingSeq: rec.pendingSeq, ConfirmedSeq: rec.confirmedSeq,
		ArrivalCount: rec.arrivalCount, Pruned: rec.pruned,
		Fenced: rec.fenced, FencedSeq: rec.fencedSeq,
	}))
}

// streamName names a stream state file from its epoch and opaque stream id.
func streamName(sk streamKey) string {
	return fmt.Sprintf("%d-%s", sk.epoch, hex.EncodeToString(sk.stream[:]))
}

func (d *dirStore) getObject(name string) *object { return d.readObject(d.path("objects", name)) }

func (d *dirStore) readObject(file string) *object {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		d.t.Fatalf("synthledger: object read: %v", err)
	}
	return d.decodeObject(raw)
}

func (d *dirStore) decodeObject(raw []byte) *object {
	var of objectFile
	if err := json.Unmarshal(raw, &of); err != nil {
		d.t.Fatalf("synthledger: object does not parse: %v", err)
	}
	key, err := keyFromName(of.Key)
	if err != nil {
		d.t.Fatalf("synthledger: stored key name: %v", err)
	}
	stream, err := decodeID[erasureledger.StreamID](of.Stream)
	if err != nil {
		d.t.Fatalf("synthledger: stored stream name: %v", err)
	}
	body, err := hex.DecodeString(of.Body)
	if err != nil {
		d.t.Fatalf("synthledger: stored body is not hex: %v", err)
	}
	state := statePending
	if of.Confirmed {
		state = stateConfirmed
	}
	return &object{
		name: of.Key, key: key, body: body,
		epoch: of.Epoch, stream: stream, seq: of.Seq,
		arrival: arrivalEvidence{clockNanos: of.ClockNanos, arrivalSeq: of.ArrivalSeq, checksum: of.Checksum},
		state:   state,
		keep:    of.Keep,
	}
}

// saveObjectOnce is the create-once arm: the file is opened with O_EXCL, so the
// second create for a key is the filesystem's refusal, and the bytes of the
// first create are never opened for writing.
func (d *dirStore) saveObjectOnce(obj *object) error {
	file := d.path("objects", obj.name)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return fmt.Errorf("%w: %s", errObjectExists, obj.name)
	}
	if err != nil {
		d.t.Fatalf("synthledger: exclusive create: %v", err)
	}
	if _, err := f.Write(d.json(d.fileOf(obj))); err != nil {
		d.closeAfterFailure(f)
		d.t.Fatalf("synthledger: exclusive create write: %v", err)
	}
	if err := f.Close(); err != nil {
		d.t.Fatalf("synthledger: exclusive create close: %v", err)
	}
	return nil
}

func (d *dirStore) saveObject(obj *object) {
	d.write(d.path("objects", obj.name), d.json(d.fileOf(obj)))
}

// fileOf renders an object for the medium. The name is the opaque key, so a
// file name is no more revealing than a listing row.
func (d *dirStore) fileOf(obj *object) objectFile {
	return objectFile{
		Key: obj.name, Body: hex.EncodeToString(obj.body),
		Epoch: obj.epoch, Stream: hex.EncodeToString(obj.stream[:]), Seq: obj.seq,
		ClockNanos: obj.arrival.clockNanos, ArrivalSeq: obj.arrival.arrivalSeq,
		Checksum: obj.arrival.checksum, Confirmed: obj.state == stateConfirmed,
		Keep: obj.keep,
	}
}

func (d *dirStore) deleteObject(name string) {
	if err := os.Remove(d.path("objects", name)); err != nil && !os.IsNotExist(err) {
		d.t.Fatalf("synthledger: object delete: %v", err)
	}
}

func (d *dirStore) page(after string, limit int) ([]*object, string) {
	ents, err := os.ReadDir(d.path("objects"))
	if err != nil {
		d.t.Fatalf("synthledger: listing read: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, ent := range ents {
		if !ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	slices.Sort(names)
	out := make([]*object, 0, limit)
	last := ""
	for _, name := range names {
		obj := d.getObject(name)
		if obj == nil || obj.state != stateConfirmed {
			// An unconfirmed write is not in the ledger and is not listable.
			continue
		}
		last = name
		if name <= after || len(out) == limit {
			continue
		}
		out = append(out, obj)
	}
	return out, last
}

func (d *dirStore) quarantineObject(obj *object) {
	d.write(d.path("quarantine", obj.name), d.json(d.fileOf(obj)))
}

func (d *dirStore) quarantinedObjects() []*object {
	ents, err := os.ReadDir(d.path("quarantine"))
	if err != nil {
		d.t.Fatalf("synthledger: quarantine read: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, ent := range ents {
		names = append(names, ent.Name())
	}
	slices.Sort(names)
	out := make([]*object, 0, len(names))
	for _, name := range names {
		if obj := d.readObject(d.path("quarantine", name)); obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

// writeMarker is the marker arm's create-once: a second sentinel for the same
// key is the filesystem's refusal.
func (d *dirStore) writeMarker(name string, obj *object) bool {
	f, err := os.OpenFile(d.path("markers", name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return false
	}
	if err != nil {
		d.t.Fatalf("synthledger: marker create: %v", err)
	}
	if _, err := f.Write(d.json(d.fileOf(obj))); err != nil {
		d.closeAfterFailure(f)
		d.t.Fatalf("synthledger: marker write: %v", err)
	}
	if err := f.Close(); err != nil {
		d.t.Fatalf("synthledger: marker close: %v", err)
	}
	return true
}

func (d *dirStore) readMarker(name string) *object {
	return d.readObject(d.path("markers", name))
}

func (d *dirStore) keepMarker(class string) string {
	raw, err := os.ReadFile(d.path("keeps", hex.EncodeToString([]byte(class))))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		d.t.Fatalf("synthledger: keep marker read: %v", err)
	}
	var kf keepFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		d.t.Fatalf("synthledger: keep marker does not parse: %v", err)
	}
	return kf.Name
}

func (d *dirStore) saveKeepMarker(class, name string) {
	d.write(d.path("keeps", hex.EncodeToString([]byte(class))), d.json(keepFile{Name: name}))
}

// closeAfterFailure closes a file whose write already failed, so the reported
// failure is the write and not a close that follows it.
func (d *dirStore) closeAfterFailure(f *os.File) {
	if err := f.Close(); err != nil {
		d.t.Fatalf("synthledger: close after a failed write: %v", err)
	}
}

// write puts a file atomically, so the medium never shows a half-written
// object to a later read.
func (d *dirStore) write(file string, data []byte) {
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		d.t.Fatalf("synthledger: write %s: %v", file, err)
	}
	if err := os.Rename(tmp, file); err != nil {
		d.t.Fatalf("synthledger: rename %s: %v", file, err)
	}
}

// json renders a value, and fails the test rather than returning a
// half-written file to a later read.
func (d *dirStore) json(v any) []byte {
	d.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		d.t.Fatalf("synthledger: encode %T: %v", v, err)
	}
	return raw
}

// idArray is the constraint a fixed-length opaque identity satisfies.
type idArray interface {
	~[16]byte
}

// decodeID restores a fixed-length opaque identity from its hex name.
func decodeID[T idArray](name string) (T, error) {
	var out T
	raw, err := hex.DecodeString(name)
	if err != nil {
		return out, fmt.Errorf("synthledger: identity name is not hex: %w", err)
	}
	if len(raw) != len(out) {
		return out, fmt.Errorf("synthledger: identity name is %d bytes, want %d", len(raw), len(out))
	}
	copy(out[:], raw)
	return out, nil
}
