package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// The server never sets it; the performance fixture needs more contacts than
// the shipped bound to measure account-scale behavior.
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

// ContactImportInput is one phone-book entry sent by contacts.importContacts.
type ContactImportInput struct {
	ClientID int64
	Phone    string
}

// ContactImportMatch maps an imported phone-book entry to an account.
type ContactImportMatch struct {
	ClientID int64
	UserID   int64
	Existing bool
	Mutual   bool
}

// ContactImportResult contains the permitted matches and client IDs postponed
// because the caller's import lookup budget is exhausted.
type ContactImportResult struct {
	Matches        []ContactImportMatch
	RetryClientIDs []int64
}

func validContactIDs(ownerID, contactID int64) bool {
	return ownerID > 0 && contactID > 0 && ownerID != contactID
}

// ImportContacts matches a phone book in one batch, applies each target's
// added-by-phone privacy rule, and adds all permitted accounts atomically.
func (s *Store) ImportContacts(ctx context.Context, ownerID int64, inputs []ContactImportInput) (ContactImportResult, error) {
	if ownerID <= 0 {
		return ContactImportResult{}, ErrInvalidContact
	}
	if len(inputs) > MaxContactsPerUser {
		return ContactImportResult{}, ErrContactLimit
	}
	if len(inputs) == 0 {
		return ContactImportResult{Matches: []ContactImportMatch{}}, nil
	}

	clientIDs := make([]int64, len(inputs))
	phones := make([]string, len(inputs))
	for i, input := range inputs {
		clientIDs[i] = input.ClientID
		phones[i] = NormalizePhone(input.Phone)
	}

	// Persist the quota charge before attempting contact mutations. A rejected
	// contact-list-cap operation must not roll back phone probes already made.
	chargeTx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContactImportResult{}, fmt.Errorf("import contacts quota begin: %w", err)
	}
	defer func() { _ = chargeTx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, chargeTx, ownerID); err != nil {
		return ContactImportResult{}, err
	}

	// Charge distinct probes in one batch statement. The owner lock serializes
	// the quota check with contact writes and other lookup surfaces.
	withinBudget, retryClientIDs, err := chargeContactImportLookups(ctx, chargeTx, ownerID, clientIDs, phones)
	if err != nil {
		return ContactImportResult{}, err
	}
	if err := chargeTx.Commit(ctx); err != nil {
		return ContactImportResult{}, fmt.Errorf("import contacts quota commit: %w", err)
	}
	var eligibleClientIDs []int64
	var eligiblePhones []string
	for i, allowed := range withinBudget {
		if !allowed {
			continue
		}
		eligibleClientIDs = append(eligibleClientIDs, clientIDs[i])
		eligiblePhones = append(eligiblePhones, phones[i])
	}
	if len(eligiblePhones) == 0 {
		return ContactImportResult{}, ErrLookupQuotaExceeded
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContactImportResult{}, fmt.Errorf("import contacts begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, tx, ownerID); err != nil {
		return ContactImportResult{}, err
	}

	// Resolve permitted phone matches in one statement snapshot. This same
	// predicate is used by contacts.resolvePhone.
	matches, err := matchContactPhones(ctx, tx, ownerID, eligibleClientIDs, eligiblePhones, false)
	if err != nil {
		return ContactImportResult{}, err
	}
	newIDs := make([]int64, 0, len(matches))
	seenUsers := make(map[int64]struct{}, len(matches))
	for _, match := range matches {
		if _, seen := seenUsers[match.UserID]; seen {
			continue
		}
		seenUsers[match.UserID] = struct{}{}
		if !match.Existing {
			newIDs = append(newIDs, match.UserID)
		}
	}
	newIDs = ascendingUnique(newIDs)

	if len(newIDs) > 0 {
		qtx := s.q.WithTx(tx)
		state, err := qtx.CountContactsForImport(ctx, ownerID)
		if err != nil {
			return ContactImportResult{}, fmt.Errorf("import contacts read owner state: %w", err)
		}
		if state.ContactCount+int64(len(newIDs)) > int64(s.maxContactsPerUser) {
			return ContactImportResult{}, ErrContactLimit
		}

		var changedViewers []int64
		if state.LastSeenContactsRule {
			inputsByViewer, err := lastSeenPrivacyInputsForViewers(ctx, qtx, ownerID, newIDs)
			if err != nil {
				return ContactImportResult{}, fmt.Errorf("import contacts evaluate status access: %w", err)
			}
			changedViewers, err = changedLastSeenAccessViewers(inputsByViewer, func(_ int64, inputs *lastSeenPrivacyInputs) {
				inputs.ownerContact = true
			})
			if err != nil {
				return ContactImportResult{}, fmt.Errorf("import contacts compare status access: %w", err)
			}
		}

		copyRows := make([][]any, len(newIDs))
		for i, id := range newIDs {
			copyRows[i] = []any{ownerID, id}
		}
		inserted, err := tx.CopyFrom(ctx, pgx.Identifier{"user_contacts"}, []string{"owner_id", "contact_id"}, pgx.CopyFromRows(copyRows))
		if err != nil {
			return ContactImportResult{}, fmt.Errorf("import contacts copy edges: %w", err)
		}
		if inserted != int64(len(newIDs)) {
			return ContactImportResult{}, fmt.Errorf("import contacts inserted %d of %d edges", inserted, len(newIDs))
		}
		if err := notifyStatusReconcile(ctx, tx, ownerID, changedViewers...); err != nil {
			return ContactImportResult{}, fmt.Errorf("import contacts notify status: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ContactImportResult{}, fmt.Errorf("import contacts commit: %w", err)
	}
	return ContactImportResult{Matches: matches, RetryClientIDs: retryClientIDs}, nil
}

func chargeContactImportLookups(ctx context.Context, tx pgx.Tx, ownerID int64, clientIDs []int64, phones []string) ([]bool, []int64, error) {
	rows, err := tx.Query(ctx, `
		WITH requested AS (
			SELECT entry.client_id, entry.phone, entry.ordinality
			FROM UNNEST($2::BIGINT[], $3::TEXT[]) WITH ORDINALITY
				AS entry(client_id, phone, ordinality)
		), contact_phones AS (
			SELECT DISTINCT requested.phone
			FROM requested
			JOIN LATERAL (
				SELECT u.id FROM users u WHERE u.phone = requested.phone LIMIT 1
			) target ON true
			JOIN user_contacts existing
			  ON existing.owner_id = $1
			 AND existing.contact_id = target.id
		), probes AS (
			SELECT DISTINCT requested.phone
			FROM requested
			WHERE NOT EXISTS (
				SELECT 1 FROM contact_phones existing
				WHERE existing.phone = requested.phone
			)
		), expired AS (
			DELETE FROM phone_lookups
			WHERE caller_id = $1
			  AND lookup_type = 'contacts_import'
			  AND looked_up_at < $4
			RETURNING phone
		), current_lookups AS (
			SELECT DISTINCT phone
			FROM phone_lookups
			WHERE caller_id = $1
			  AND lookup_type = 'contacts_import'
			  AND looked_up_at >= $4
		), quota AS (
			SELECT COUNT(*)::BIGINT AS used FROM current_lookups
		), new_probes AS (
			SELECT probes.phone
			FROM probes
			WHERE NOT EXISTS (
				SELECT 1 FROM current_lookups current
				WHERE current.phone = probes.phone
			)
		), ranked AS (
			SELECT phone, ROW_NUMBER() OVER (ORDER BY phone) AS position
			FROM new_probes
		), accepted AS (
			SELECT ranked.phone
			FROM ranked CROSS JOIN quota
			WHERE quota.used + ranked.position <= $5
		), inserted AS (
			INSERT INTO phone_lookups (caller_id, lookup_type, phone, looked_up_at)
			SELECT $1, 'contacts_import', accepted.phone, now()
			FROM accepted
			RETURNING phone
		), eligible AS (
			SELECT phone FROM contact_phones
			UNION
			SELECT phone FROM current_lookups
			UNION
			SELECT phone FROM inserted
		)
		SELECT requested.ordinality, eligible.phone IS NOT NULL
		FROM requested
		LEFT JOIN eligible USING (phone)
		ORDER BY requested.ordinality
	`, ownerID, clientIDs, phones, time.Now().Add(-LookupWindow), ContactImportLookupLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("charge contact import lookups: %w", err)
	}
	defer rows.Close()
	withinBudget := make([]bool, len(clientIDs))
	retryClientIDs := make([]int64, 0)
	var rowCount int
	for rows.Next() {
		var ordinality int64
		var allowed bool
		if err := rows.Scan(&ordinality, &allowed); err != nil {
			return nil, nil, fmt.Errorf("charge contact import lookups scan: %w", err)
		}
		if ordinality < 1 || ordinality > int64(len(clientIDs)) {
			return nil, nil, fmt.Errorf("charge contact import lookups: invalid input position %d", ordinality)
		}
		index := int(ordinality - 1)
		withinBudget[index] = allowed
		if !allowed {
			retryClientIDs = append(retryClientIDs, clientIDs[index])
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("charge contact import lookups: %w", err)
	}
	if rowCount != len(clientIDs) {
		return nil, nil, fmt.Errorf("charge contact import lookups: got %d results for %d inputs", rowCount, len(clientIDs))
	}
	return withinBudget, retryClientIDs, nil
}

type contactPhoneQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func matchContactPhones(ctx context.Context, queryer contactPhoneQueryer, ownerID int64, clientIDs []int64, phones []string, includeOwner bool) ([]ContactImportMatch, error) {
	rows, err := queryer.Query(ctx, `
		WITH requested AS (
			SELECT entry.client_id, entry.phone, entry.ordinality
			FROM UNNEST($2::BIGINT[], $3::TEXT[]) WITH ORDINALITY
				AS entry(client_id, phone, ordinality)
		)
		SELECT requested.client_id, target.id, existing.contact_id IS NOT NULL, target_contact.contact_id IS NOT NULL
		FROM requested
		-- A plain join hash-joins users and reads every account; the LIMIT keeps
		-- this a per-phone probe of users_phone_unique.
		JOIN LATERAL (
			SELECT u.id FROM users u WHERE u.phone = requested.phone LIMIT 1
		) target ON true
		LEFT JOIN user_privacy_rules privacy
		  ON privacy.owner_id = target.id
		 AND privacy.privacy_key = 'added_by_phone'
		LEFT JOIN user_privacy_rule_users disallowed
		  ON disallowed.owner_id = target.id
		 AND disallowed.privacy_key = 'added_by_phone'
		 AND disallowed.user_id = $1
		 AND disallowed.entry_type = 'disallow'
		LEFT JOIN user_privacy_rule_users allowed
		  ON allowed.owner_id = target.id
		 AND allowed.privacy_key = 'added_by_phone'
		 AND allowed.user_id = $1
		 AND allowed.entry_type = 'allow'
		LEFT JOIN user_contacts target_contact
		  ON target_contact.owner_id = target.id
		 AND target_contact.contact_id = $1
		LEFT JOIN user_contacts existing
		  ON existing.owner_id = $1
		 AND existing.contact_id = target.id
		WHERE (target.id <> $1 OR $4::BOOLEAN)
		  AND (
			target.id = $1
			OR (
				disallowed.user_id IS NULL
				AND (
					allowed.user_id IS NOT NULL
					OR COALESCE(privacy.base_rule, 'everybody') = 'everybody'
					OR (privacy.base_rule = 'contacts' AND target_contact.contact_id IS NOT NULL)
				)
			)
		  )
		ORDER BY requested.ordinality
	`, ownerID, clientIDs, phones, includeOwner)
	if err != nil {
		return nil, fmt.Errorf("match contact phones: %w", err)
	}
	defer rows.Close()
	var matches []ContactImportMatch
	for rows.Next() {
		var match ContactImportMatch
		if err := rows.Scan(&match.ClientID, &match.UserID, &match.Existing, &match.Mutual); err != nil {
			return nil, fmt.Errorf("match contact phones scan: %w", err)
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("match contact phones: %w", err)
	}
	return matches, nil
}

// UserByPhoneForCaller returns a phone match only when the target permits the
// caller under its added-by-phone privacy rule. The same predicate gates batch
// imports.
func (s *Store) UserByPhoneForCaller(ctx context.Context, callerID int64, phone string) (User, bool, error) {
	if callerID <= 0 {
		return User{}, false, ErrInvalidContact
	}
	matches, err := matchContactPhones(ctx, s.pool, callerID, []int64{0}, []string{NormalizePhone(phone)}, true)
	if err != nil {
		return User{}, false, err
	}
	if len(matches) == 0 {
		return User{}, false, nil
	}
	user, ok, err := s.UserByID(ctx, matches[0].UserID)
	if err != nil {
		return User{}, false, fmt.Errorf("load permitted phone match: %w", err)
	}
	return user, ok, nil
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
