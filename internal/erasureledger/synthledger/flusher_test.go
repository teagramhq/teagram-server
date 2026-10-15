package synthledger_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/erasureledger/synthledger"
)

// arms runs fn over a fresh provider on each storage arm.
func arms(t *testing.T, fn func(t *testing.T, p *synthledger.Provider)) {
	t.Helper()
	for _, arm := range []struct {
		name string
		open func(testing.TB) synthledger.Store
	}{
		{"memory", synthledger.NewMemoryStore},
		{"dir", synthledger.OpenDirStore},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fn(t, synthledger.New(t, arm.open(t)))
		})
	}
}

func streamOf(b byte) erasureledger.StreamID {
	var s erasureledger.StreamID
	s[0] = b
	return s
}

func keyOf(b byte) erasureledger.OperationKey {
	var k erasureledger.OperationKey
	k[0], k[15] = 0x5a, b
	return k
}

// frame encodes an account record at one position under key, with id making
// the payload, and so the bytes, distinct.
func frame(t *testing.T, stream erasureledger.StreamID, seq int64, key erasureledger.OperationKey, id int64) []byte {
	t.Helper()
	payload, err := erasureledger.NewAccount(id)
	if err != nil {
		t.Fatalf("NewAccount(%d): %v", id, err)
	}
	rec, err := erasureledger.NewRecord(payload.Kind(), 1, stream, seq, key, payload)
	if err != nil {
		t.Fatalf("NewRecord seq=%d: %v", seq, err)
	}
	body, err := erasureledger.Encode(rec)
	if err != nil {
		t.Fatalf("Encode seq=%d: %v", seq, err)
	}
	return body
}

func at(stream erasureledger.StreamID, seq int64) synthledger.Position {
	return synthledger.Position{Epoch: 1, Stream: stream, Seq: seq}
}

func create(t *testing.T, f synthledger.Flusher, body []byte, want synthledger.Answer) {
	t.Helper()
	got, err := f.Create(body)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got != want {
		t.Fatalf("Create answer = %v, want %v", got, want)
	}
}

func TestFlusherAndProofReaderAreSingleCapabilities(t *testing.T) {
	t.Parallel()
	p := synthledger.New(t, synthledger.NewMemoryStore(t))
	for _, tc := range []struct {
		principal any
		want      []string
	}{
		{p.NewFlusher(1, streamOf(1)), []string{"Create"}},
		{p.ProofReader(), []string{"Proof"}},
	} {
		typ := reflect.TypeOf(tc.principal)
		var got []string
		for m := range typ.Methods() {
			got = append(got, m.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s methods = %v, want %v", typ.Name(), got, tc.want)
		}
	}
	// A proof is the provider's: no caller can fill one in.
	for f := range reflect.TypeFor[synthledger.Proof]().Fields() {
		if f.IsExported() {
			t.Errorf("Proof.%s is exported", f.Name)
		}
	}
}

func TestFlusherStoresBytesAsGiven(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stream := streamOf(1)
		body := frame(t, stream, 1, keyOf(1), 10)
		given := slices.Clone(body)
		create(t, p.NewFlusher(1, stream), body, synthledger.AnswerCreated)
		body[len(body)-1] ^= 0xff
		held, ok := p.StoredBody(synthledger.KeyName(keyOf(1)))
		if !ok || !bytes.Equal(held, given) {
			t.Fatalf("held bytes = %x, want the bytes given to create %x", held, given)
		}
	})
}

func TestFlusherSequenceGate(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stream := streamOf(1)
		f, r := p.NewFlusher(1, stream), p.ProofReader()
		one, two := frame(t, stream, 1, keyOf(1), 10), frame(t, stream, 2, keyOf(2), 20)

		create(t, f, two, synthledger.AnswerUnresolvedPredecessor)
		if _, err := r.Proof(at(stream, 2)); !errors.Is(err, synthledger.ErrNotFound) {
			t.Fatalf("proof after a gated create: err = %v, want not-found", err)
		}
		create(t, f, one, synthledger.AnswerCreated)
		create(t, f, two, synthledger.AnswerCreated)
	})
}

func TestFlusherGateOffAcceptsAnyOrderAndLogsIt(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stream := streamOf(1)
		f := p.NewFlusher(1, stream)
		p.DisableSequenceGate()
		create(t, f, frame(t, stream, 2, keyOf(2), 20), synthledger.AnswerCreated)
		create(t, f, frame(t, stream, 1, keyOf(1), 10), synthledger.AnswerCreated)
		want := []synthledger.Position{at(stream, 2), at(stream, 1)}
		if got := p.CreateLog(); !slices.Equal(got, want) {
			t.Fatalf("create log = %v, want %v", got, want)
		}
	})
}

func TestFlusherTypedAnswers(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stream := streamOf(1)
		f, r := p.NewFlusher(1, stream), p.ProofReader()
		original := frame(t, stream, 1, keyOf(1), 10)
		create(t, f, original, synthledger.AnswerCreated)

		create(t, f, original, synthledger.AnswerMatchingProof)
		create(t, f, frame(t, stream, 1, keyOf(1), 11), synthledger.AnswerConflictingProof)
		create(t, f, frame(t, stream, 2, keyOf(1), 10), synthledger.AnswerConflictingProof)
		create(t, f, frame(t, stream, 1, keyOf(9), 10), synthledger.AnswerLostLineage)

		// None of the refused creates changed what the position holds, and the
		// proof names the held key, not the colliding one just passed in.
		proof, err := r.Proof(at(stream, 1))
		if err != nil {
			t.Fatalf("Proof: %v", err)
		}
		if proof.Key() != keyOf(1) || proof.Checksum() != sha256.Sum256(original) {
			t.Fatalf("proof = key %x checksum %x, want the original key and SHA-256 of its bytes",
				proof.Key(), proof.Checksum())
		}
	})
}

func TestDroppedResponseStillProves(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stream := streamOf(1)
		f, r := p.NewFlusher(1, stream), p.ProofReader()
		body := frame(t, stream, 1, keyOf(1), 10)
		p.DropResponse(at(stream, 1))
		if answer, err := f.Create(body); !errors.Is(err, synthledger.ErrResponseLost) || answer != synthledger.AnswerNone {
			t.Fatalf("Create with a dropped response = %v, %v; want no answer and ErrResponseLost", answer, err)
		}
		proof, err := r.Proof(at(stream, 1))
		if err != nil {
			t.Fatalf("Proof after a dropped response: %v", err)
		}
		if proof.Key() != keyOf(1) || proof.Checksum() != sha256.Sum256(body) || proof.ArrivalSeq() < 1 {
			t.Fatalf("proof = %x/%x/%d, want key, SHA-256 of the bytes and an arrival sequence",
				proof.Key(), proof.Checksum(), proof.ArrivalSeq())
		}
		if proof.Position() != at(stream, 1) {
			t.Errorf("proof position = %v, want %v", proof.Position(), at(stream, 1))
		}
		// The retry of the lost create is the same write.
		create(t, f, body, synthledger.AnswerMatchingProof)
		again, err := r.Proof(at(stream, 1))
		if err != nil || again != proof {
			t.Fatalf("proof after the retry = %v, %v; want it unchanged", again, err)
		}
	})
}

func TestStalledCreateHoldsOnlyItself(t *testing.T) {
	t.Parallel()
	arms(t, func(t *testing.T, p *synthledger.Provider) {
		t.Helper()
		stalled, other := streamOf(1), streamOf(2)
		release := make(chan struct{})
		entered := p.StallCreate(at(stalled, 1), release)

		done := make(chan synthledger.Answer)
		go func() {
			answer, err := p.NewFlusher(1, stalled).Create(frame(t, stalled, 1, keyOf(1), 10))
			if err != nil {
				t.Errorf("stalled Create: %v", err)
			}
			done <- answer
		}()
		<-entered

		// While the create is held, the provider has not acted on it, and other
		// calls proceed.
		create(t, p.NewFlusher(1, other), frame(t, other, 1, keyOf(2), 20), synthledger.AnswerCreated)
		if _, err := p.ProofReader().Proof(at(stalled, 1)); !errors.Is(err, synthledger.ErrNotFound) {
			t.Fatalf("proof of the held create: err = %v, want not-found", err)
		}
		close(release)
		if answer := <-done; answer != synthledger.AnswerCreated {
			t.Fatalf("released create answer = %v, want created", answer)
		}
	})
}

func TestProviderIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	p := synthledger.New(t, synthledger.NewMemoryStore(t))
	const streams, depth = 8, 16
	var wg sync.WaitGroup
	for s := range streams {
		stream := streamOf(byte(s + 1))
		f, r := p.NewFlusher(1, stream), p.ProofReader()
		bodies := make([][]byte, depth)
		for seq := range depth {
			bodies[seq] = frame(t, stream, int64(seq+1), keyOf(byte(s*depth+seq)), int64(seq+1))
		}
		wg.Go(func() {
			for seq, body := range bodies {
				if answer, err := f.Create(body); err != nil || answer != synthledger.AnswerCreated {
					t.Errorf("stream %d seq %d: %v, %v", s, seq+1, answer, err)
					return
				}
				if _, err := r.Proof(at(stream, int64(seq+1))); err != nil {
					t.Errorf("stream %d seq %d proof: %v", s, seq+1, err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range depth {
			if _, err := p.Replayer().List("", 4); err != nil {
				t.Errorf("List: %v", err)
			}
			p.CreateLog()
		}
	})
	wg.Wait()
	if got := len(p.CreateLog()); got != streams*depth {
		t.Fatalf("create log has %d entries, want %d", got, streams*depth)
	}
}
