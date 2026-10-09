package erasureledger

type streamBindingKey struct {
	epoch  int64
	stream StreamID
}

// StreamBindings holds only confirmed immutable ownership declarations. It is
// an inert evidence index for synthetic readers; it performs no provider I/O,
// persistence, replay, or admission work.
type StreamBindings struct {
	lineages map[streamBindingKey]LineageID
}

// NewStreamBindings returns an empty confirmed-binding index.
func NewStreamBindings() *StreamBindings {
	return &StreamBindings{lineages: make(map[streamBindingKey]LineageID)}
}

// AddConfirmed adds a binding record after its provider confirmation. The
// first binding for a stream starts at sequence one. An identical binding is
// idempotent; a different lineage for the same (epoch, stream) refuses
// readiness and leaves the first binding intact.
func (b *StreamBindings) AddConfirmed(record Record) error {
	if b == nil {
		return newRejected("binding index is nil", "bindings")
	}
	if err := record.validate(); err != nil {
		return err
	}
	if record.Kind != KindStreamBinding {
		return newRejected("record is not a stream binding", "kind")
	}
	binding, ok := record.Payload.(StreamBinding)
	if !ok {
		return newRejected("binding payload has the wrong type", "payload")
	}
	key := streamBindingKey{epoch: record.Epoch, stream: record.Stream}
	if lineage, found := b.lineages[key]; found {
		if lineage != binding.Lineage {
			return newContractNotReady(CauseBindingConflict, KindStreamBinding)
		}
		return nil
	}
	if record.Seq != 1 {
		return newContractNotReady(CauseBindingOrder, KindStreamBinding)
	}
	if b.lineages == nil {
		b.lineages = make(map[streamBindingKey]LineageID)
	}
	b.lineages[key] = binding.Lineage
	return nil
}

// LineageFor attributes a record through its confirmed (epoch, stream)
// binding. Bodies that carry their own lineage must agree with that binding.
func (b *StreamBindings) LineageFor(record Record) (LineageID, error) {
	if err := record.validate(); err != nil {
		return LineageID{}, err
	}
	if b == nil {
		return LineageID{}, newContractNotReady(CauseMissingBinding, record.Kind)
	}
	key := streamBindingKey{epoch: record.Epoch, stream: record.Stream}
	lineage, found := b.lineages[key]
	if !found {
		return LineageID{}, newContractNotReady(CauseMissingBinding, record.Kind)
	}
	switch payload := record.Payload.(type) {
	case StreamBinding:
		if payload.Lineage != lineage {
			return LineageID{}, newContractNotReady(CauseBindingConflict, KindStreamBinding)
		}
	case Epoch:
		if payload.Number != record.Epoch || payload.Lineage != lineage {
			return LineageID{}, newContractNotReady(CauseBindingMismatch, KindEpoch)
		}
	}
	return lineage, nil
}

// ComponentKeyFor derives the exact component identity for a component
// reservation. The returned key keeps equal allocator names on separate
// streams and lineages distinct.
func (b *StreamBindings) ComponentKeyFor(record Record) (ComponentKey, error) {
	if record.Kind != KindComponentReservation {
		return ComponentKey{}, newRejected("record is not a component reservation", "kind")
	}
	reservation, ok := record.Payload.(ComponentReservation)
	if !ok {
		return ComponentKey{}, newRejected("component reservation payload has the wrong type", "payload")
	}
	lineage, err := b.LineageFor(record)
	if err != nil {
		return ComponentKey{}, err
	}
	return NewComponentKey(lineage, record.Stream, reservation.Allocator)
}
