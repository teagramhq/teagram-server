package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ErrInvalidContact is returned when a contact edge has a non-positive id or
// points from a user to themself.
var ErrInvalidContact = errors.New("invalid contact")

// ErrContactLimit is returned when an owner already has MaxContactsPerUser
// contacts and tries to add a new one.
var ErrContactLimit = errors.New("contact limit reached")

// MaxContactsPerUser is the shipped maximum number of directed contacts one
// account may store.
const MaxContactsPerUser = 5000

// WithMaxContactsPerUser replaces the per-account contact bound for one Store.
// This process-level option can only be set by trusted code constructing the
// Store; request data cannot change it. The server keeps the shipped 5,000
// contact bound, while the performance fixture raises it to 15,000 to admit
// 10,000 seeded contacts plus 5,000 imported book users. A higher bound allows
// each account to persist more contact edges, increasing database use and the
// work of contact-list and reconciliation operations; it does not change
// contact access or privacy rules. Non-positive values are ignored.
func WithMaxContactsPerUser(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxContactsPerUser = n
		}
	}
}

// Contact is one owner-scoped contact edge. Mutual is derived from the
// recipient's reverse edge when the list is read; it is never stored.
type Contact struct {
	UserID int64
	Mutual bool
}

// ContactWithUser is a contact edge, its rendered user record, and visible
// current profile photo, if any.
type ContactWithUser struct {
	Contact Contact
	User    User
	PhotoID int64
}

func validContactIDs(ownerID, contactID int64) bool {
	return ownerID > 0 && contactID > 0 && ownerID != contactID
}

// AddContact adds contactID to ownerID's directed contact list. The existing
// owner advisory lock serializes list-cap checks and mutations for that owner.
func (s *Store) AddContact(ctx context.Context, ownerID, contactID int64) (changed bool, err error) {
	if !validContactIDs(ownerID, contactID) {
		return false, ErrInvalidContact
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, tx, ownerID); err != nil {
		return false, err
	}
	qtx := s.q.WithTx(tx)
	exists, err := qtx.ContactExists(ctx, db.ContactExistsParams{
		OwnerID:   ownerID,
		ContactID: contactID,
	})
	if err != nil {
		return false, fmt.Errorf("check contact: %w", err)
	}
	if exists {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit: %w", err)
		}
		return false, nil
	}
	count, err := qtx.CountContacts(ctx, ownerID)
	if err != nil {
		return false, fmt.Errorf("count contacts: %w", err)
	}
	if count >= int64(s.maxContactsPerUser) {
		return false, ErrContactLimit
	}
	inputsByViewer, err := lastSeenPrivacyInputsForViewers(ctx, qtx, ownerID, []int64{contactID})
	if err != nil {
		return false, fmt.Errorf("evaluate status access before adding contact: %w", err)
	}
	changedViewers, err := changedLastSeenAccessViewers(inputsByViewer, func(_ int64, inputs *lastSeenPrivacyInputs) {
		inputs.ownerContact = true
	})
	if err != nil {
		return false, fmt.Errorf("compare status access after adding contact: %w", err)
	}
	n, err := qtx.InsertContact(ctx, db.InsertContactParams{
		OwnerID:   ownerID,
		ContactID: contactID,
	})
	if err != nil {
		return false, fmt.Errorf("insert contact: %w", err)
	}
	if n > 0 {
		if err := notifyStatusReconcile(ctx, tx, ownerID, changedViewers...); err != nil {
			return false, fmt.Errorf("notify status after adding contact: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return n > 0, nil
}

// RemoveContact deletes only ownerID's directed edge to contactID. An absent
// edge is a successful no-op.
func (s *Store) RemoveContact(ctx context.Context, ownerID, contactID int64) (changed bool, err error) {
	if !validContactIDs(ownerID, contactID) {
		return false, ErrInvalidContact
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, tx, ownerID); err != nil {
		return false, err
	}
	qtx := s.q.WithTx(tx)
	inputsByViewer, err := lastSeenPrivacyInputsForViewers(ctx, qtx, ownerID, []int64{contactID})
	if err != nil {
		return false, fmt.Errorf("evaluate status access before deleting contact: %w", err)
	}
	changedViewers, err := changedLastSeenAccessViewers(inputsByViewer, func(_ int64, inputs *lastSeenPrivacyInputs) {
		inputs.ownerContact = false
	})
	if err != nil {
		return false, fmt.Errorf("compare status access after deleting contact: %w", err)
	}
	n, err := qtx.DeleteContact(ctx, db.DeleteContactParams{
		OwnerID:   ownerID,
		ContactID: contactID,
	})
	if err != nil {
		return false, fmt.Errorf("delete contact: %w", err)
	}
	if n > 0 {
		if err := notifyStatusReconcile(ctx, tx, ownerID, changedViewers...); err != nil {
			return false, fmt.Errorf("notify status after deleting contact: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return n > 0, nil
}

// RemoveContacts deletes an owner's selected contact edges in one transaction.
// The entire id set is validated before the transaction can mutate anything.
func (s *Store) RemoveContacts(ctx context.Context, ownerID int64, contactIDs []int64) (changed bool, err error) {
	if ownerID <= 0 {
		return false, ErrInvalidContact
	}
	for _, contactID := range contactIDs {
		if !validContactIDs(ownerID, contactID) {
			return false, ErrInvalidContact
		}
	}
	if len(contactIDs) == 0 {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, tx, ownerID); err != nil {
		return false, err
	}
	qtx := s.q.WithTx(tx)
	deletedContactIDs, err := contactIDsToDelete(ctx, tx, ownerID, contactIDs)
	if err != nil {
		return false, err
	}
	inputsByViewer, err := lastSeenPrivacyInputsForViewers(ctx, qtx, ownerID, deletedContactIDs)
	if err != nil {
		return false, fmt.Errorf("evaluate status access before deleting contacts: %w", err)
	}
	changedViewers, err := changedLastSeenAccessViewers(inputsByViewer, func(_ int64, inputs *lastSeenPrivacyInputs) {
		inputs.ownerContact = false
	})
	if err != nil {
		return false, fmt.Errorf("compare status access after deleting contacts: %w", err)
	}
	n, err := qtx.DeleteContacts(ctx, db.DeleteContactsParams{
		OwnerID:    ownerID,
		ContactIds: contactIDs,
	})
	if err != nil {
		return false, fmt.Errorf("delete contacts: %w", err)
	}
	if n > 0 {
		if err := notifyStatusReconcile(ctx, tx, ownerID, changedViewers...); err != nil {
			return false, fmt.Errorf("notify status after deleting contacts: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return n > 0, nil
}

func contactIDsToDelete(ctx context.Context, tx pgx.Tx, ownerID int64, contactIDs []int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT contact_id
		FROM user_contacts
		WHERE owner_id = $1 AND contact_id = ANY($2::BIGINT[])
		ORDER BY contact_id
	`, ownerID, contactIDs)
	if err != nil {
		return nil, fmt.Errorf("list contacts before deleting: %w", err)
	}
	defer rows.Close()
	var deletedIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list contacts before deleting scan: %w", err)
		}
		deletedIDs = append(deletedIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list contacts before deleting: %w", err)
	}
	return deletedIDs, nil
}

// IsContact reports whether ownerID has a directed edge to contactID.
func (s *Store) IsContact(ctx context.Context, ownerID, contactID int64) (bool, error) {
	if !validContactIDs(ownerID, contactID) {
		return false, ErrInvalidContact
	}
	exists, err := s.q.ContactExists(ctx, db.ContactExistsParams{
		OwnerID:   ownerID,
		ContactID: contactID,
	})
	if err != nil {
		return false, fmt.Errorf("check contact: %w", err)
	}
	return exists, nil
}

// Contacts returns an owner's contacts in contact-id order and the same
// statement's total. Mutual state is derived from each contact's reverse edge.
func (s *Store) Contacts(ctx context.Context, ownerID int64) ([]Contact, int, error) {
	rows, err := s.q.ListContacts(ctx, ownerID)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}
	contacts := make([]Contact, len(rows))
	var total int
	for i, row := range rows {
		contacts[i] = Contact{UserID: row.ContactID, Mutual: row.Mutual}
		if i == 0 {
			total = int(row.Total)
		}
	}
	return contacts, total, nil
}

// ContactsWithUsers loads a full contact page, user records, and visible
// current profile photos in one query. It is intended for full-list responses;
// hash-only checks should use Contacts so they do not hydrate users
// unnecessarily.
func (s *Store) ContactsWithUsers(ctx context.Context, ownerID int64) ([]ContactWithUser, int, error) {
	rows, err := s.q.ContactsWithUsers(ctx, ownerID)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts with users: %w", err)
	}
	if len(rows) == 0 {
		return []ContactWithUser{}, 0, nil
	}
	total := int(rows[0].Total)
	if len(rows) != total {
		return nil, 0, fmt.Errorf("list contacts with users: got %d rows for %d contacts", len(rows), total)
	}
	contacts := make([]ContactWithUser, len(rows))
	for i, row := range rows {
		var photoID int64
		if row.PhotoFileID != nil {
			photoID = *row.PhotoFileID
		}
		contacts[i] = ContactWithUser{
			Contact: Contact{UserID: row.ContactID, Mutual: row.Mutual},
			User: UserFromDB(db.UserByIDRow{
				ID:         row.ID,
				FirstName:  row.FirstName,
				LastName:   row.LastName,
				IsOnline:   row.IsOnline,
				LastSeenAt: row.LastSeenAt,
				Username:   row.Username,
				Kind:       row.Kind,
			}),
			PhotoID: photoID,
		}
	}
	return contacts, total, nil
}

// ContactStates returns the owner's existing edges within contactIDs and each
// edge's live reciprocal state. Unmatched IDs have no contact edge.
func (s *Store) ContactStates(ctx context.Context, ownerID int64, contactIDs []int64) ([]Contact, error) {
	if ownerID <= 0 {
		return nil, ErrInvalidContact
	}
	for _, contactID := range contactIDs {
		if !validContactIDs(ownerID, contactID) {
			return nil, ErrInvalidContact
		}
	}
	if len(contactIDs) == 0 {
		return []Contact{}, nil
	}
	rows, err := s.q.ContactStates(ctx, db.ContactStatesParams{
		OwnerID:    ownerID,
		ContactIds: contactIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list contact states: %w", err)
	}
	contacts := make([]Contact, len(rows))
	for i, row := range rows {
		contacts[i] = Contact{UserID: row.ContactID, Mutual: row.Mutual}
	}
	return contacts, nil
}
