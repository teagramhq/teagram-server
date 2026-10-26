package e2e_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeContactsImport(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const ownerPhone, targetPhone, missingPhone = "+15551099101", "+15551099102", "+15551099103"
	seedSmokeUsers(t, f, ownerPhone)
	owner := newSmokeClient(t, f, "Contacts import owner", ownerPhone)

	target, err := f.store.CreateUser(f.ctx, targetPhone)
	if err != nil {
		t.Fatalf("create phone-matched target: %v", err)
	}
	firstName, lastName := "Global", "Target"
	if err := f.store.UpdateProfile(f.ctx, target.ID, &firstName, &lastName); err != nil {
		t.Fatalf("set target profile: %v", err)
	}
	if _, err := f.store.AddContact(f.ctx, target.ID, owner.id); err != nil {
		t.Fatalf("make owner a contact of target: %v", err)
	}

	var imported *tg.ContactsImportedContacts
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		imported, err = client.ContactsImportContacts(ctx, []tg.InputPhoneContact{
			{ClientID: 101, Phone: targetPhone, FirstName: "Local", LastName: "Name"},
			{ClientID: 102, Phone: missingPhone, FirstName: "Missing", LastName: "Contact"},
		})
		return err
	}); err != nil {
		t.Fatalf("contacts.importContacts: %v", err)
	}
	if imported == nil {
		t.Fatal("contacts.importContacts returned nil")
	}
	if len(imported.Imported) != 1 || imported.Imported[0].UserID != target.ID || imported.Imported[0].ClientID != 101 {
		t.Fatalf("imported contacts = %+v, want target %d for client id 101", imported.Imported, target.ID)
	}
	if len(imported.PopularInvites) != 0 || len(imported.RetryContacts) != 0 {
		t.Fatalf("popular invites/retry contacts = %d/%d, want empty", len(imported.PopularInvites), len(imported.RetryContacts))
	}
	if len(imported.Users) != 1 {
		t.Fatalf("imported users = %d, want one", len(imported.Users))
	}
	user, ok := imported.Users[0].(*tg.User)
	if !ok || user.ID != target.ID || user.Phone != "" || user.FirstName != firstName || user.LastName != lastName || !user.Contact || !user.MutualContact {
		t.Fatalf("imported user = %#v, want sanitized target %d with global name", imported.Users[0], target.ID)
	}

	var contacts tg.ContactsContactsClass
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		contacts, err = client.ContactsGetContacts(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("contacts.getContacts after import: %v", err)
	}
	list, ok := contacts.(*tg.ContactsContacts)
	if !ok || list.SavedCount != 1 || len(list.Contacts) != 1 || list.Contacts[0].UserID != target.ID {
		t.Fatalf("contacts after import = %#v, want the matching target only", contacts)
	}

	tooMany := make([]tg.InputPhoneContact, store.MaxContactsPerUser+1)
	for i := range tooMany {
		tooMany[i] = tg.InputPhoneContact{ClientID: int64(i + 1), Phone: "+15551999999"}
	}
	err = owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsImportContacts(ctx, tooMany)
		return err
	})
	if !tgerr.Is(err, "CONTACTS_TOO_MUCH") {
		t.Fatalf("contacts.importContacts over limit error = %v, want CONTACTS_TOO_MUCH", err)
	}
}

func testSmokeContactsImportPrivacy(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const ownerPhone, deniedPhone, allowedPhone, contactPhone = "+15551099201", "+15551099202", "+15551099203", "+15551099204"
	owner := newSmokePhoneClientWithRequests(t, f, "Contacts import privacy owner", ownerPhone, nil)
	denied := newSmokePhoneClientWithRequests(t, f, "Contacts import privacy denied", deniedPhone, nil)
	allowed := newSmokePhoneClientWithRequests(t, f, "Contacts import privacy allowed", allowedPhone, nil)
	contactAllowed := newSmokePhoneClientWithRequests(t, f, "Contacts import privacy contacts", contactPhone, nil)
	if _, err := f.store.AddContact(f.ctx, contactAllowed.id, owner.id); err != nil {
		t.Fatalf("make importer a contact of target: %v", err)
	}

	for _, setting := range []struct {
		client *smokeClient
		rules  []tg.InputPrivacyRuleClass
	}{
		{
			client: denied,
			rules: []tg.InputPrivacyRuleClass{
				&tg.InputPrivacyValueAllowAll{},
				&tg.InputPrivacyValueDisallowUsers{Users: []tg.InputUserClass{inputUser(denied.id, owner.id)}},
			},
		},
		{
			client: allowed,
			rules: []tg.InputPrivacyRuleClass{
				&tg.InputPrivacyValueDisallowAll{},
				&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(allowed.id, owner.id)}},
			},
		},
		{
			client: contactAllowed,
			rules:  []tg.InputPrivacyRuleClass{&tg.InputPrivacyValueAllowContacts{}},
		},
	} {
		if err := setSmokePrivacyRequest(f.ctx, setting.client, &tg.InputPrivacyKeyAddedByPhone{}, setting.rules...); err != nil {
			t.Fatalf("set added-by-phone privacy for %d: %v", setting.client.id, err)
		}
	}
	for _, target := range []struct {
		client   *smokeClient
		baseRule string
		allow    bool
		disallow bool
	}{
		{client: denied, baseRule: "everybody", disallow: true},
		{client: allowed, baseRule: "nobody", allow: true},
		{client: contactAllowed, baseRule: "contacts"},
	} {
		rule, exists, err := f.store.GetAddedByPhonePrivacy(f.ctx, target.client.id)
		if err != nil || !exists || rule.BaseRule != target.baseRule {
			t.Fatalf("added-by-phone privacy for %d = %+v exists=%v err=%v", target.client.id, rule, exists, err)
		}
		if target.allow && (len(rule.AllowedUserIDs) != 1 || rule.AllowedUserIDs[0] != owner.id) {
			t.Fatalf("added-by-phone allow users for %d = %v, want owner %d", target.client.id, rule.AllowedUserIDs, owner.id)
		}
		if target.disallow && (len(rule.DisallowedUserIDs) != 1 || rule.DisallowedUserIDs[0] != owner.id) {
			t.Fatalf("added-by-phone disallow users for %d = %v, want owner %d", target.client.id, rule.DisallowedUserIDs, owner.id)
		}
	}
	privacy, err := getSmokePrivacy(f.ctx, allowed, &tg.InputPrivacyKeyAddedByPhone{})
	if err != nil || len(privacy.Rules) != 2 {
		t.Fatalf("get added-by-phone privacy = %#v err=%v, want base and allow exception", privacy, err)
	}
	if _, ok := privacy.Rules[0].(*tg.PrivacyValueDisallowAll); !ok {
		t.Fatalf("added-by-phone base rule = %T, want *tg.PrivacyValueDisallowAll", privacy.Rules[0])
	}
	allow, ok := privacy.Rules[1].(*tg.PrivacyValueAllowUsers)
	if !ok || len(allow.Users) != 1 || allow.Users[0] != owner.id {
		t.Fatalf("added-by-phone allow rule = %#v, want owner %d", privacy.Rules[1], owner.id)
	}

	var imported *tg.ContactsImportedContacts
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		imported, err = client.ContactsImportContacts(ctx, []tg.InputPhoneContact{
			{ClientID: 201, Phone: deniedPhone, FirstName: "Denied", LastName: "By Rule"},
			{ClientID: 202, Phone: allowedPhone, FirstName: "Allowed", LastName: "By Exception"},
			{ClientID: 203, Phone: contactPhone, FirstName: "Allowed", LastName: "By Contacts"},
		})
		return err
	}); err != nil {
		t.Fatalf("contacts.importContacts with privacy rules: %v", err)
	}
	if len(imported.Imported) != 2 || imported.Imported[0].UserID != allowed.id || imported.Imported[0].ClientID != 202 || imported.Imported[1].UserID != contactAllowed.id || imported.Imported[1].ClientID != 203 {
		t.Fatalf("privacy-filtered imports = %+v, want allowed user %d/client 202 and contacts user %d/client 203", imported.Imported, allowed.id, contactAllowed.id)
	}
	if len(imported.Users) != 2 {
		t.Fatalf("privacy-filtered users = %d, want two", len(imported.Users))
	}
	exists, err := f.store.IsContact(f.ctx, owner.id, denied.id)
	if err != nil {
		t.Fatalf("check denied contact edge: %v", err)
	}
	if exists {
		t.Fatal("denied target was added to the contact list")
	}
}

func testSmokeContactsImportBudget(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const ownerPhone, missingPhone, targetPhone = "+15551099301", "+15551099302", "+15551099303"
	seedSmokeUsers(t, f, ownerPhone)
	owner := newSmokeClient(t, f, "Contacts import budget owner", ownerPhone)
	target, err := f.store.CreateUser(f.ctx, targetPhone)
	if err != nil {
		t.Fatalf("create target for over-budget import: %v", err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to fixture database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close fixture database connection: %v", err)
		}
	})
	if _, err := conn.Exec(f.ctx, `
		INSERT INTO phone_lookups (caller_id, lookup_type, phone, looked_up_at)
		SELECT $1, 'contacts_import', 'budget-seed-' || n, now()
		FROM generate_series(1, $2) AS n
	`, owner.id, store.MaxContactsPerUser-1); err != nil {
		t.Fatalf("seed contact import quota: %v", err)
	}

	importContacts := func(inputs ...tg.InputPhoneContact) (*tg.ContactsImportedContacts, error) {
		t.Helper()
		var imported *tg.ContactsImportedContacts
		err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			var err error
			imported, err = client.ContactsImportContacts(ctx, inputs)
			return err
		})
		return imported, err
	}
	initial, err := importContacts(tg.InputPhoneContact{ClientID: 301, Phone: missingPhone})
	if err != nil {
		t.Fatalf("charge last import slot with missing phone: %v", err)
	}
	if len(initial.Imported) != 0 || len(initial.RetryContacts) != 0 {
		t.Fatalf("initial missing-phone result imported=%+v retry=%v, want empty", initial.Imported, initial.RetryContacts)
	}

	resync, err := importContacts(tg.InputPhoneContact{ClientID: 302, Phone: missingPhone})
	if err != nil {
		t.Fatalf("resync an already-probed phone: %v", err)
	}
	if len(resync.Imported) != 0 || len(resync.RetryContacts) != 0 {
		t.Fatalf("resync result imported=%+v retry=%v, want empty", resync.Imported, resync.RetryContacts)
	}

	partial, err := importContacts(
		tg.InputPhoneContact{ClientID: 303, Phone: missingPhone},
		tg.InputPhoneContact{ClientID: 304, Phone: targetPhone},
	)
	if err != nil {
		t.Fatalf("import with one cached and one over-budget phone: %v", err)
	}
	if len(partial.Imported) != 0 || len(partial.RetryContacts) != 1 || partial.RetryContacts[0] != 304 {
		t.Fatalf("partial import = imported %+v retry %v, want no matches and retry client 304", partial.Imported, partial.RetryContacts)
	}
	if exists, err := f.store.IsContact(f.ctx, owner.id, target.ID); err != nil || exists {
		t.Fatalf("over-budget target contact exists=%v err=%v, want absent", exists, err)
	}

	_, err = importContacts(tg.InputPhoneContact{ClientID: 305, Phone: targetPhone})
	if !tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("all-over-budget import error = %v, want FLOOD_WAIT", err)
	}
}
