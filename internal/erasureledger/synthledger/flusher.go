package synthledger

// The flusher and the proof reader are the byte-level pair a store-side flush
// uses. The flusher hands the provider a stored outbox record exactly as the
// database holds it, and gets back a typed answer about the position. The
// answer is never proof: proof of arrival comes only from the proof reader,
// which reports what the provider holds at a position, with the key read from
// the held bytes, the provider's arrival sequence, and a SHA-256 the provider
// computes over the held bytes at read time.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// ErrResponseLost is the dropped-response fault: the provider acted on the
// create, and its answer never reached the caller.
var ErrResponseLost = errors.New("synthledger: create response lost")

// Position is one (epoch, stream, seq) slot on a replica stream.
type Position struct {
	Epoch  int64
	Stream erasureledger.StreamID
	Seq    int64
}

// Answer is the flusher's typed answer about a position.
type Answer uint8

const (
	// AnswerNone is the zero answer, carried by a refusal or a lost response.
	AnswerNone Answer = iota
	// AnswerCreated means the provider stored the bytes at the position.
	AnswerCreated
	// AnswerMatchingProof means the position already holds the same key with
	// the same bytes and checksum: a repeat of an earlier create.
	AnswerMatchingProof
	// AnswerConflictingProof means the provider holds the same key with
	// different bytes or a different checksum. Nothing was overwritten.
	AnswerConflictingProof
	// AnswerLostLineage means the position holds a different operation key.
	AnswerLostLineage
	// AnswerUnresolvedPredecessor means the provider's sequence gate refused
	// the create because the stream holds nothing at seq-1.
	AnswerUnresolvedPredecessor
)

func (a Answer) String() string {
	switch a {
	case AnswerNone:
		return "none"
	case AnswerCreated:
		return "created"
	case AnswerMatchingProof:
		return "matching proof"
	case AnswerConflictingProof:
		return "conflicting proof"
	case AnswerLostLineage:
		return "lost lineage"
	case AnswerUnresolvedPredecessor:
		return "unresolved predecessor"
	}
	return fmt.Sprintf("answer(%d)", uint8(a))
}

// Flusher is the byte-level write credential for one (epoch, replica stream).
// Create is its whole capability.
type Flusher struct {
	p      *Provider
	epoch  int64
	stream erasureledger.StreamID
}

// NewFlusher mints a flusher credential bound to one (epoch, replica stream).
func (p *Provider) NewFlusher(epoch int64, stream erasureledger.StreamID) Flusher {
	return Flusher{p: p, epoch: epoch, stream: stream}
}

// Create stores body, an encoded ledger record, at the position its envelope
// names. The provider decodes the bytes only to read the envelope; what it
// stores and checksums is body as given.
func (f Flusher) Create(body []byte) (Answer, error) {
	return f.p.flush(f, body)
}

func (p *Provider) flush(f Flusher, body []byte) (Answer, error) {
	rec, err := erasureledger.Decode(body)
	if err != nil {
		return AnswerNone, fmt.Errorf("%w: bytes do not decode: %w", ErrCredential, err)
	}
	if rec.Epoch != f.epoch || rec.Stream != f.stream {
		return AnswerNone, fmt.Errorf("%w: envelope epoch and stream are not the credential's", ErrCredential)
	}
	if rec.OpKey == (erasureledger.OperationKey{}) {
		return AnswerNone, fmt.Errorf("%w: the record carries no operation key", ErrCredential)
	}
	pos := Position{Epoch: rec.Epoch, Stream: rec.Stream, Seq: rec.Seq}
	p.waitStall(pos)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pos)
	answer, err := p.storeAt(pos, rec.OpKey, slices.Clone(body))
	if p.drops[pos] {
		delete(p.drops, pos)
		return AnswerNone, fmt.Errorf("%w: position %d", ErrResponseLost, pos.Seq)
	}
	return answer, err
}

// waitStall holds a create whose position has a stall armed, outside the
// provider lock, so every other call proceeds while it waits.
func (p *Provider) waitStall(pos Position) {
	p.mu.Lock()
	st, ok := p.stalls[pos]
	delete(p.stalls, pos)
	p.mu.Unlock()
	if !ok {
		return
	}
	close(st.entered)
	<-st.release
}

func (p *Provider) storeAt(pos Position, key erasureledger.OperationKey, body []byte) (Answer, error) {
	sk := streamKey{epoch: pos.Epoch, stream: pos.Stream}
	if held := p.s.objectAt(sk, pos.Seq); held != nil {
		switch {
		case held.key != key:
			return AnswerLostLineage, nil
		case slices.Equal(held.body, body) && held.arrival.checksum == sha256.Sum256(body):
			return AnswerMatchingProof, nil
		default:
			return AnswerConflictingProof, nil
		}
	}
	name := KeyName(key)
	if p.s.getObject(name) != nil {
		// The key is held at another position, so its bytes differ.
		return AnswerConflictingProof, nil
	}
	str, _ := p.s.streamState(sk)
	if str.fenced && pos.Seq > str.fencedSeq {
		p.s.quarantineObject(&object{
			name: name, key: key, body: body,
			epoch: pos.Epoch, stream: pos.Stream, seq: pos.Seq,
			arrival: p.arriveAt(body), state: stateConfirmed,
		})
		return AnswerNone, fmt.Errorf("%w: stream %s is fenced at sequence %d",
			ErrQuarantined, KeyName(pos.Stream), str.fencedSeq)
	}
	if !p.gateOff && pos.Seq > 1 && pos.Seq-1 > str.pruned && p.s.objectAt(sk, pos.Seq-1) == nil {
		return AnswerUnresolvedPredecessor, nil
	}
	obj := &object{
		name: name, key: key, body: body,
		epoch: pos.Epoch, stream: pos.Stream, seq: pos.Seq,
		arrival: p.arriveAt(body), state: statePending,
	}
	if err := p.s.saveObjectOnce(obj); err != nil {
		return AnswerNone, err
	}
	if err := p.publish(obj); err != nil {
		return AnswerNone, err
	}
	str.confirmedSeq = max(str.confirmedSeq, pos.Seq)
	str.arrivalCount++
	p.s.saveStreamState(sk, str)
	p.verifs = append(p.verifs, verifierEntry{
		epoch: pos.Epoch, stream: pos.Stream,
		count: str.arrivalCount, high: str.confirmedSeq,
	})
	return AnswerCreated, nil
}

// StallCreate holds the next flusher create at pos until release closes. The
// returned channel closes when that create arrives at the provider.
func (p *Provider) StallCreate(pos Position, release <-chan struct{}) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	entered := make(chan struct{})
	p.stalls[pos] = stall{entered: entered, release: release}
	return entered
}

// DropResponse loses the answer of the next flusher create at pos: the
// provider acts on the create, and the caller gets ErrResponseLost.
func (p *Provider) DropResponse(pos Position) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drops[pos] = true
}

// DisableSequenceGate makes the flusher accept creates in any order.
func (p *Provider) DisableSequenceGate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gateOff = true
}

// CreateLog is every flusher create, in the order it reached the provider.
func (p *Provider) CreateLog() []Position {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// ProofReader is the independent verifier read by position. Proof is its
// whole capability.
type ProofReader struct{ p *Provider }

// ProofReader mints the proof reader credential.
func (p *Provider) ProofReader() ProofReader { return ProofReader{p: p} }

// Proof reports what the provider holds at pos.
func (r ProofReader) Proof(pos Position) (Proof, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.proof(pos)
}

func (p *Provider) proof(pos Position) (Proof, error) {
	held := p.s.objectAt(streamKey{epoch: pos.Epoch, stream: pos.Stream}, pos.Seq)
	if held == nil {
		return Proof{}, fmt.Errorf("%w: nothing held at position %d", ErrNotFound, pos.Seq)
	}
	rec, err := erasureledger.Decode(held.body)
	if err != nil {
		return Proof{}, fmt.Errorf("synthledger: held bytes do not decode: %w", err)
	}
	if rec.Epoch != pos.Epoch || rec.Stream != pos.Stream || rec.Seq != pos.Seq || rec.OpKey != held.key {
		return Proof{}, errors.New("synthledger: held bytes do not name the position they are stored at")
	}
	return Proof{
		pos: pos, key: rec.OpKey,
		arrivalSeq: held.arrival.arrivalSeq,
		checksum:   sha256.Sum256(held.body),
	}, nil
}

// Proof is the provider's account of one position. Its fields are unexported,
// so only the provider makes one: the key is read from the held bytes, the
// arrival sequence is the provider's, and the checksum is computed over the
// held bytes when the proof is read.
type Proof struct {
	pos        Position
	key        erasureledger.OperationKey
	arrivalSeq int64
	checksum   [sha256.Size]byte
}

// Position is the slot the proof describes.
func (p Proof) Position() Position { return p.pos }

// Key is the operation key the held bytes carry.
func (p Proof) Key() erasureledger.OperationKey { return p.key }

// ArrivalSeq is the provider's arrival sequence for the held object.
func (p Proof) ArrivalSeq() int64 { return p.arrivalSeq }

// Checksum is the SHA-256 of the held bytes.
func (p Proof) Checksum() [sha256.Size]byte { return p.checksum }
