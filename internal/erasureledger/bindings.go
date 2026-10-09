package erasureledger

type streamBindingKey struct {
	epoch  int64
	stream StreamID
}

// StreamBindings holds only confirmed immutable ownership declarations. It is
// an inert evidence index for synthetic readers; it performs no provider I/O,
// persistence, replay, or admission work.
type StreamBindings struct {
	lineages   map[streamBindingKey]LineageID
	observed   map[streamBindingKey]map[LineageID]struct{}
	conflicted map[streamBindingKey]struct{}
}

// NewStreamBindings returns an empty confirmed-binding index.
func NewStreamBindings() *StreamBindings {
	return &StreamBindings{
		lineages:   make(map[streamBindingKey]LineageID),
		observed:   make(map[streamBindingKey]map[LineageID]struct{}),
		conflicted: make(map[streamBindingKey]struct{}),
	}
}

// AddConfirmed adds a binding record after its provider confirmation. The
// first accepted binding for a stream starts at sequence one. Confirmed
// lineages are remembered even when they arrive out of order, so conflicting
// ownership refuses readiness regardless of arrival order. Re-adding the
// accepted lineage is idempotent and never clears a conflict.
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
	if b.lineages == nil {
		b.lineages = make(map[streamBindingKey]LineageID)
	}
	if b.observed == nil {
		b.observed = make(map[streamBindingKey]map[LineageID]struct{})
	}
	if b.conflicted == nil {
		b.conflicted = make(map[streamBindingKey]struct{})
	}
	if b.observed[key] == nil {
		b.observed[key] = make(map[LineageID]struct{})
	}
	b.observed[key][binding.Lineage] = struct{}{}
	if len(b.observed[key]) > 1 {
		b.conflicted[key] = struct{}{}
	}
	if _, conflicted := b.conflicted[key]; conflicted {
		if lineage, accepted := b.lineages[key]; accepted && lineage == binding.Lineage {
			return nil
		}
		return newContractNotReady(CauseBindingConflict, KindStreamBinding)
	}
	if lineage, accepted := b.lineages[key]; accepted {
		if lineage == binding.Lineage {
			return nil
		}
		return newContractNotReady(CauseBindingConflict, KindStreamBinding)
	}
	if record.Seq != 1 {
		return newContractNotReady(CauseBindingOrder, KindStreamBinding)
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
	if _, conflicted := b.conflicted[key]; conflicted {
		return LineageID{}, newContractNotReady(CauseBindingConflict, record.Kind)
	}
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
