package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	maxDefaultDialogPins = 5
	maxDialogPinOrder    = 100
)

var (
	ErrDialogPinPeerInvalid  = errors.New("dialog pin peer is inaccessible")
	ErrDialogPinsTooMuch     = errors.New("default dialog pin limit reached")
	ErrDialogPinOrderTooLong = errors.New("dialog pin order is too long")
)

// DialogPinPeer identifies a peer that can be pinned in the default folder.
type DialogPinPeer struct {
	PeerType PeerType
	PeerID   int64
}

// DialogPin is one owner-scoped default-folder pin in display order.
type DialogPin struct {
	PeerType PeerType
	PeerID   int64
	Position int
}

// DialogPinsPeerSnapshot keeps the active pin order and rendered peer dialogs
// inside one repeatable-read snapshot.
type DialogPinsPeerSnapshot struct {
	Pins        []DialogPin
	PeerDialogs PeerDialogsSnapshot
}

// DialogPinsPeerSnapshot returns the caller's currently visible default-folder
// pins with the peer dialogs and update state rendered from the same snapshot.
func (s *Store) DialogPinsPeerSnapshot(ctx context.Context, ownerID int64, now time.Time) (DialogPinsPeerSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DialogPinsPeerSnapshot{}, fmt.Errorf("begin dialog pins snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	rows, err := qtx.ListDialogPins(ctx, ownerID)
	if err != nil {
		return DialogPinsPeerSnapshot{}, fmt.Errorf("list dialog pins: %w", err)
	}
	pins := make([]DialogPin, 0, len(rows))
	for _, row := range rows {
		if row.Position == nil {
			return DialogPinsPeerSnapshot{}, errors.New("stored dialog pin has no position")
		}
		pin := DialogPin{PeerType: PeerType(row.PeerType), PeerID: row.PeerID, Position: int(*row.Position)}
		pins = append(pins, pin)
	}
	accessible, err := dialogPinAccessiblePeers(ctx, qtx, ownerID, dialogPinPeerKeys(pins), now)
	if err != nil {
		return DialogPinsPeerSnapshot{}, err
	}
	visiblePins := make([]DialogPin, 0, len(pins))
	visiblePeers := make([]PeerDialogKey, 0, len(pins))
	for _, pin := range pins {
		key := DialogPinPeer{PeerType: pin.PeerType, PeerID: pin.PeerID}
		if !accessible[key] {
			continue
		}
		visiblePins = append(visiblePins, pin)
		visiblePeers = append(visiblePeers, PeerDialogKey{PeerType: pin.PeerType, PeerID: pin.PeerID})
	}
	peerSnapshot, err := s.peerDialogsSnapshotInTx(ctx, tx, ownerID, visiblePeers, true)
	if err != nil {
		return DialogPinsPeerSnapshot{}, err
	}
	// A pin is only useful when the peer still has a rendered dialog. The
	// access predicate already requires this for users and groups; this final
	// check keeps the wire response bounded by the actual snapshot rows.
	present := make(map[DialogPinPeer]bool, len(peerSnapshot.Dialogs))
	for _, dialog := range peerSnapshot.Dialogs {
		present[DialogPinPeer{PeerType: dialog.Dialog.PeerType, PeerID: dialog.Dialog.PeerID}] = true
	}
	filteredPins := make([]DialogPin, 0, len(visiblePins))
	for _, pin := range visiblePins {
		if present[DialogPinPeer{PeerType: pin.PeerType, PeerID: pin.PeerID}] {
			filteredPins = append(filteredPins, pin)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DialogPinsPeerSnapshot{}, fmt.Errorf("commit dialog pins snapshot: %w", err)
	}
	return DialogPinsPeerSnapshot{Pins: filteredPins, PeerDialogs: peerSnapshot}, nil
}

// DialogPinChangeAt returns the persistent marker time, including after the
// final value row has been removed.
func (s *Store) DialogPinChangeAt(ctx context.Context, ownerID int64) (time.Time, bool, error) {
	changedAt, err := s.q.DialogPinChangeAt(ctx, ownerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("read dialog pin marker: %w", err)
	case !changedAt.Valid:
		return time.Time{}, false, nil
	default:
		return changedAt.Time, true, nil
	}
}

// DialogsWithPins returns one dialog page with active default-folder pin flags.
// excludePinned applies the same live-membership predicate before paging.
func (s *Store) DialogsWithPins(ctx context.Context, ownerID, offsetID int64, limit int, excludePinned bool) ([]Dialog, error) {
	rows, err := s.q.DialogsForOwnerWithPins(ctx, db.DialogsForOwnerWithPinsParams{
		OwnerID: ownerID, OffsetID: offsetID, ExcludePinned: excludePinned,
		Lim: int32(limit), //nolint:gosec // limit is validated and bounded by the handler
	})
	if err != nil {
		return nil, fmt.Errorf("dialogs for owner with pins: %w", err)
	}
	out := make([]Dialog, len(rows))
	for i, row := range rows {
		out[i] = Dialog{
			OwnerID: row.OwnerID, PeerType: PeerType(row.PeerType), PeerID: row.PeerID,
			TopMessage: row.TopMessage, UnreadCount: int(row.UnreadCount),
			ReadInboxMaxID: row.ReadInboxMaxID, ReadOutboxMaxID: row.ReadOutboxMaxID,
			Pinned: row.Pinned != nil && *row.Pinned,
		}
	}
	return out, nil
}

// CountDialogsWithPins returns the full list count used by getDialogs' slice
// response when excludePinned is enabled.
func (s *Store) CountDialogsWithPins(ctx context.Context, ownerID int64, excludePinned bool) (int, error) {
	if !excludePinned {
		return s.CountDialogs(ctx, ownerID)
	}
	count, err := s.q.CountDialogsExcludingPinned(ctx, ownerID)
	if err != nil {
		return 0, fmt.Errorf("count unpinned dialogs for owner: %w", err)
	}
	return int(count), nil
}

// ToggleDialogPin applies one owner-scoped toggle after current peer access is
// checked under the same owner advisory lock used by membership removal.
func (s *Store) ToggleDialogPin(ctx context.Context, ownerID int64, peer DialogPinPeer, pinned bool, now time.Time) (bool, error) {
	if ownerID <= 0 || !validDialogPinPeer(peer) {
		return false, ErrDialogPinPeerInvalid
	}
	tx, qtx, err := s.beginDialogPinMutation(ctx, ownerID)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	current, err := loadDialogPins(ctx, qtx, ownerID)
	if err != nil {
		return false, err
	}
	accessPeers := append(slices.Clone(current), peer)
	accessible, err := dialogPinAccessiblePeers(ctx, qtx, ownerID, accessPeers, now)
	if err != nil {
		return false, err
	}
	if !accessible[peer] {
		return false, ErrDialogPinPeerInvalid
	}
	active := accessibleDialogPins(current, accessible)
	pruned := len(active) != len(current)
	desired := slices.Clone(active)
	index := dialogPinIndex(desired, peer)
	if pinned {
		if index < 0 {
			if len(desired) >= maxDefaultDialogPins {
				return false, ErrDialogPinsTooMuch
			}
			desired = append([]DialogPinPeer{peer}, desired...)
		}
	} else if index >= 0 {
		desired = append(desired[:index], desired[index+1:]...)
	}
	changed := pruned || !sameDialogPinOrder(active, desired)
	if err := finishDialogPinMutation(ctx, tx, qtx, ownerID, desired, changed); err != nil {
		return false, err
	}
	return changed, nil
}

// ReorderDialogPins applies force replacement or front-moving reorder semantics.
// Input entries are already syntactically decoded; every peer is authorized
// here with the mutation transaction still holding the owner's lock.
func (s *Store) ReorderDialogPins(ctx context.Context, ownerID int64, order []DialogPinPeer, force bool, now time.Time) (bool, error) {
	if len(order) > maxDialogPinOrder {
		return false, ErrDialogPinOrderTooLong
	}
	order = uniqueDialogPinPeers(order)
	if ownerID <= 0 {
		return false, ErrDialogPinPeerInvalid
	}
	tx, qtx, err := s.beginDialogPinMutation(ctx, ownerID)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	current, err := loadDialogPins(ctx, qtx, ownerID)
	if err != nil {
		return false, err
	}
	peers := append(slices.Clone(current), order...)
	accessible, err := dialogPinAccessiblePeers(ctx, qtx, ownerID, peers, now)
	if err != nil {
		return false, err
	}
	for _, peer := range order {
		if !validDialogPinPeer(peer) || !accessible[peer] {
			return false, ErrDialogPinPeerInvalid
		}
	}
	if force && len(order) > maxDefaultDialogPins {
		return false, ErrDialogPinsTooMuch
	}
	active := accessibleDialogPins(current, accessible)
	pruned := len(active) != len(current)
	var desired []DialogPinPeer
	if force {
		desired = slices.Clone(order)
	} else {
		listed := make(map[DialogPinPeer]bool, len(order))
		for _, peer := range order {
			listed[peer] = true
			if dialogPinIndex(active, peer) >= 0 {
				desired = append(desired, peer)
			}
		}
		for _, peer := range active {
			if !listed[peer] {
				desired = append(desired, peer)
			}
		}
	}
	if len(desired) > maxDefaultDialogPins {
		return false, ErrDialogPinsTooMuch
	}
	changed := pruned || !sameDialogPinOrder(active, desired)
	if err := finishDialogPinMutation(ctx, tx, qtx, ownerID, desired, changed); err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) beginDialogPinMutation(ctx context.Context, ownerID int64) (pgx.Tx, *db.Queries, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin dialog pin mutation: %w", err)
	}
	if err := lockOwners(ctx, tx, ownerID); err != nil {
		_ = tx.Rollback(context.Background()) //nolint:errcheck // best effort on the error path
		return nil, nil, fmt.Errorf("lock dialog pin owner: %w", err)
	}
	if s.dialogPinMutationHook != nil {
		s.dialogPinMutationHook()
	}
	return tx, s.q.WithTx(tx), nil
}

func loadDialogPins(ctx context.Context, qtx *db.Queries, ownerID int64) ([]DialogPinPeer, error) {
	rows, err := qtx.ListDialogPins(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list dialog pins for mutation: %w", err)
	}
	peers := make([]DialogPinPeer, 0, len(rows))
	for _, row := range rows {
		if row.Position == nil {
			return nil, errors.New("stored dialog pin has no position")
		}
		peers = append(peers, DialogPinPeer{PeerType: PeerType(row.PeerType), PeerID: row.PeerID})
	}
	return peers, nil
}

func dialogPinAccessiblePeers(ctx context.Context, qtx *db.Queries, ownerID int64, peers []DialogPinPeer, now time.Time) (map[DialogPinPeer]bool, error) {
	out := make(map[DialogPinPeer]bool, len(peers))
	var userIDs, chatIDs, channelIDs []int64
	for _, peer := range peers {
		out[peer] = false
		switch peer.PeerType {
		case PeerTypeUser:
			if peer.PeerID == ownerID {
				continue
			}
			userIDs = append(userIDs, peer.PeerID)
		case PeerTypeChat:
			chatIDs = append(chatIDs, peer.PeerID)
		case PeerTypeChannel:
			continue
		}
	}
	dialogPresent := make(map[DialogPinPeer]bool)
	if len(userIDs) > 0 {
		rows, err := qtx.DialogPinDialogsForOwner(ctx, db.DialogPinDialogsForOwnerParams{
			OwnerID: ownerID, PeerTypes: []int16{int16(PeerTypeUser)}, PeerIds: uniqueInt64s(userIDs),
		})
		if err != nil {
			return nil, fmt.Errorf("check existing 1:1 dialogs for pins: %w", err)
		}
		for _, row := range rows {
			dialogPresent[DialogPinPeer{PeerType: PeerType(row.PeerType), PeerID: row.PeerID}] = true
		}
	}
	for _, peer := range peers {
		switch peer.PeerType {
		case PeerTypeUser:
			if peer.PeerID == ownerID || dialogPresent[peer] {
				out[peer] = true
			}
		case PeerTypeChannel:
			channelIDs = append(channelIDs, peer.PeerID)
		}
	}
	if len(chatIDs) > 0 {
		memberIDs, err := qtx.DialogFilterChatMemberships(ctx, db.DialogFilterChatMembershipsParams{
			UserID: ownerID, ChatIds: uniqueInt64s(chatIDs),
		})
		if err != nil {
			return nil, fmt.Errorf("check current group membership for pins: %w", err)
		}
		for _, id := range memberIDs {
			out[DialogPinPeer{PeerType: PeerTypeChat, PeerID: id}] = true
		}
	}
	if len(channelIDs) > 0 {
		rows, err := qtx.DialogFilterChannelMemberships(ctx, db.DialogFilterChannelMembershipsParams{
			UserID: ownerID, ChannelIds: uniqueInt64s(channelIDs),
		})
		if err != nil {
			return nil, fmt.Errorf("check current channel membership for pins: %w", err)
		}
		for _, row := range rows {
			if channelMemberFromRow(row).Banned(now) {
				continue
			}
			out[DialogPinPeer{PeerType: PeerTypeChannel, PeerID: row.ChannelID}] = true
		}
	}
	return out, nil
}

func accessibleDialogPins(pins []DialogPinPeer, accessible map[DialogPinPeer]bool) []DialogPinPeer {
	active := make([]DialogPinPeer, 0, len(pins))
	for _, peer := range pins {
		if accessible[peer] {
			active = append(active, peer)
		}
	}
	return active
}

func dialogPinPeerKeys(pins []DialogPin) []DialogPinPeer {
	peers := make([]DialogPinPeer, len(pins))
	for i, pin := range pins {
		peers[i] = DialogPinPeer{PeerType: pin.PeerType, PeerID: pin.PeerID}
	}
	return peers
}

func validDialogPinPeer(peer DialogPinPeer) bool {
	return peer.PeerID > 0 && (peer.PeerType == PeerTypeUser || peer.PeerType == PeerTypeChat || peer.PeerType == PeerTypeChannel)
}

func uniqueDialogPinPeers(peers []DialogPinPeer) []DialogPinPeer {
	seen := make(map[DialogPinPeer]bool, len(peers))
	out := make([]DialogPinPeer, 0, len(peers))
	for _, peer := range peers {
		if seen[peer] {
			continue
		}
		seen[peer] = true
		out = append(out, peer)
	}
	return out
}

func uniqueInt64s(values []int64) []int64 {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

func dialogPinIndex(pins []DialogPinPeer, peer DialogPinPeer) int {
	for i, pin := range pins {
		if pin == peer {
			return i
		}
	}
	return -1
}

func sameDialogPinOrder(a, b []DialogPinPeer) bool {
	return slices.Equal(a, b)
}

func finishDialogPinMutation(ctx context.Context, tx pgx.Tx, qtx *db.Queries, ownerID int64, order []DialogPinPeer, changed bool) error {
	selfPeer := DialogPinPeer{PeerType: PeerTypeUser, PeerID: ownerID}
	if dialogPinIndex(order, selfPeer) >= 0 {
		if err := qtx.EnsureSelfDialogForOwner(ctx, ownerID); err != nil {
			return fmt.Errorf("ensure Saved Messages dialog for pin: %w", err)
		}
	} else if err := qtx.DeleteEmptySelfDialogForOwner(ctx, ownerID); err != nil {
		return fmt.Errorf("remove empty Saved Messages dialog after unpin: %w", err)
	}
	if !changed {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit unchanged dialog pin mutation: %w", err)
		}
		return nil
	}
	if err := qtx.DeleteDialogPinsForOwner(ctx, ownerID); err != nil {
		return fmt.Errorf("replace dialog pins: %w", err)
	}
	for position, peer := range order {
		storedPosition := int16(position)
		if err := qtx.InsertDialogPin(ctx, db.InsertDialogPinParams{
			OwnerID: ownerID, PeerType: int16(peer.PeerType), PeerID: peer.PeerID, Position: &storedPosition,
		}); err != nil {
			return fmt.Errorf("insert dialog pin at position %d: %w", position, err)
		}
	}
	if err := qtx.MarkDialogPinsChanged(ctx, ownerID); err != nil {
		return fmt.Errorf("mark dialog pins changed: %w", err)
	}
	if err := qtx.NotifyDialogPinMutation(ctx, strconv.FormatInt(ownerID, 10)); err != nil {
		return fmt.Errorf("notify dialog pin mutation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dialog pin mutation: %w", err)
	}
	return nil
}
