package repository

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/errx"
	"github.com/warmbly/warmbly/internal/models"
)

// Regression cover for issue #511: editing a contact's email address saved
// nothing. models.UpdateContact carried no Email field, so the dashboard's
// `{"email": "..."}` was dropped by the JSON decode and the PATCH answered 200
// with the old address.
//
// Run against the dev stack:
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_dev?sslmode=disable \
//	  go test ./internal/repository/ -run LiveContactEmail -v

func TestLiveContactEmailIsSavedAndResetsVerification(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	repo := NewContactRepostory(handle)
	ctx := context.Background()
	mate := f.mate.String()

	// The contact arrives with a verdict and an observation, both of which
	// belong to the address rather than to the person.
	if _, err := pool.Exec(ctx, `
		UPDATE contacts SET verification_status = 'valid', verification_sub_status = 'role',
		       verification_reason = 'accepted', verification_source = 'probe',
		       verification_provider = 'builtin', is_catch_all = true,
		       verification_checked_at = NOW(), verification_confidence = 80,
		       verification_evidence_at = NOW()
		WHERE id = $1`, f.contact); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO contact_verification_evidence (contact_id, kind, ref) VALUES ($1, 'delivered', 'i511')`,
		f.contact); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	read := func() (string, string, string, bool, bool, int16, bool, int) {
		t.Helper()
		var addr, status, source string
		var catchAll, checked, evidenceAt bool
		var confidence int16
		var evidence int
		if err := pool.QueryRow(ctx, `
			SELECT c.email, c.verification_status, c.verification_source, c.is_catch_all,
			       c.verification_checked_at IS NOT NULL, c.verification_confidence,
			       c.verification_evidence_at IS NOT NULL,
			       (SELECT COUNT(*) FROM contact_verification_evidence e WHERE e.contact_id = c.id)
			FROM contacts c WHERE c.id = $1`, f.contact).
			Scan(&addr, &status, &source, &catchAll, &checked, &confidence, &evidenceAt, &evidence); err != nil {
			t.Fatalf("read contact: %v", err)
		}
		return addr, status, source, catchAll, checked, confidence, evidenceAt, evidence
	}

	// A re-save of the address already on the row, in another case, is not a
	// change: it must not throw away a verdict the address earned.
	same := "I187-" + f.contact.String()[:8] + "@Test.Local"
	if _, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &same}); xerr != nil {
		t.Fatalf("update same address: %v", xerr)
	}
	if _, status, _, _, _, _, _, evidence := read(); status != "valid" || evidence != 1 {
		t.Fatalf("re-saving the same address reset verification: status=%q evidence=%d", status, evidence)
	}

	// The real edit. A display name and stray case are normalized away.
	next := "  Dana Reyes <Dana@Acme.Test>  "
	updated, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &next})
	if xerr != nil {
		t.Fatalf("update email: %v", xerr)
	}
	if updated.Email != "dana@acme.test" {
		t.Fatalf("response email = %q, want dana@acme.test", updated.Email)
	}
	addr, status, source, catchAll, checked, confidence, evidenceAt, evidence := read()
	if addr != "dana@acme.test" {
		t.Fatalf("stored email = %q, want dana@acme.test", addr)
	}
	if status != "unknown" || source != "" || catchAll || checked || confidence != 0 || evidenceAt {
		t.Fatalf("verification survived the address change: status=%q source=%q catch_all=%v checked=%v confidence=%d evidence_at=%v",
			status, source, catchAll, checked, confidence, evidenceAt)
	}
	if evidence != 0 {
		t.Fatalf("evidence rows after the address change = %d, want 0", evidence)
	}

	// An unrelated edit still reports the current address.
	company := "Acme"
	updated, xerr = repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Company: &company})
	if xerr != nil {
		t.Fatalf("update company: %v", xerr)
	}
	if updated.Email != "dana@acme.test" {
		t.Fatalf("email after unrelated edit = %q", updated.Email)
	}
}

func TestLiveContactEmailRefusesCollisionAndGarbage(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	repo := NewContactRepostory(handle)
	ctx := context.Background()
	mate := f.mate.String()

	other := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO contacts (id, user_id, organization_id, email, first_name, last_name, company, phone, custom_fields, updated_at, created_at)
		VALUES ($1, $2, $3, 'taken@acme.test', 'Sam', 'Ruiz', '', '', '{}'::jsonb, NOW(), NOW())`,
		other, f.owner, f.org); err != nil {
		t.Fatalf("second contact: %v", err)
	}

	taken := "Taken@Acme.test"
	_, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &taken})
	if xerr == nil || xerr.Code != errx.Conflict {
		t.Fatalf("colliding address = %v, want a 409", xerr)
	}

	for _, bad := range []string{"", "   ", "not-an-address", "dana@", "@acme.test"} {
		v := bad
		_, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &v})
		if xerr == nil || xerr.Code != errx.BadRequest {
			t.Fatalf("address %q = %v, want a 400", bad, xerr)
		}
	}

	// Nothing above may have written.
	var addr string
	if err := pool.QueryRow(ctx, `SELECT email FROM contacts WHERE id = $1`, f.contact).Scan(&addr); err != nil {
		t.Fatalf("read contact: %v", err)
	}
	if want := "i187-" + f.contact.String()[:8] + "@test.local"; addr != want {
		t.Fatalf("email = %q, want the original %q", addr, want)
	}
}
