package repository

import (
	"context"
	"strings"
	"testing"
	"time"

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
		       verification_evidence_at = NOW(), esp_provider = 'gmail', esp_resolved_at = NOW()
		WHERE id = $1`, f.contact); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO contact_verification_evidence (contact_id, kind, ref) VALUES ($1, 'delivered', 'i511')`,
		f.contact); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	type state struct {
		addr, status, source, esp string
		catchAll, checked         bool
		confidence                int16
		evidenceAt, resetAt       bool
		evidence                  int
	}
	read := func() state {
		t.Helper()
		var st state
		if err := pool.QueryRow(ctx, `
			SELECT c.email, c.verification_status, c.verification_source, c.esp_provider, c.is_catch_all,
			       c.verification_checked_at IS NOT NULL, c.verification_confidence,
			       c.verification_evidence_at IS NOT NULL, c.verification_evidence_reset_at IS NOT NULL,
			       (SELECT COUNT(*) FROM contact_verification_evidence e WHERE e.contact_id = c.id)
			FROM contacts c WHERE c.id = $1`, f.contact).
			Scan(&st.addr, &st.status, &st.source, &st.esp, &st.catchAll, &st.checked, &st.confidence,
				&st.evidenceAt, &st.resetAt, &st.evidence); err != nil {
			t.Fatalf("read contact: %v", err)
		}
		return st
	}

	// A re-save of the address already on the row, in another case, is not a
	// change: it must not throw away a verdict the address earned.
	same := "I187-" + f.contact.String()[:8] + "@Test.Local"
	if _, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &same}); xerr != nil {
		t.Fatalf("update same address: %v", xerr)
	}
	if st := read(); st.status != "valid" || st.evidence != 1 || st.resetAt {
		t.Fatalf("re-saving the same address reset verification: %+v", st)
	}

	// A stored address that predates normalization is rewritten in place, and
	// that is still not a different mailbox.
	if _, err := pool.Exec(ctx, `UPDATE contacts SET email = $2 WHERE id = $1`, f.contact, same); err != nil {
		t.Fatalf("legacy casing: %v", err)
	}
	lower := strings.ToLower(same)
	if _, xerr := repo.Update(ctx, mate, f.contact.String(), f.org, &models.UpdateContact{Email: &lower}); xerr != nil {
		t.Fatalf("normalize casing: %v", xerr)
	}
	if st := read(); st.addr != lower || st.status != "valid" || st.evidence != 1 {
		t.Fatalf("case-only save did not normalize in place: %+v", st)
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
	st := read()
	if st.addr != "dana@acme.test" {
		t.Fatalf("stored email = %q, want dana@acme.test", st.addr)
	}
	if st.status != "unknown" || st.source != "" || st.catchAll || st.checked || st.confidence != 0 || st.evidenceAt {
		t.Fatalf("verification survived the address change: %+v", st)
	}
	if st.evidence != 0 {
		t.Fatalf("evidence rows after the address change = %d, want 0", st.evidence)
	}
	if !st.resetAt {
		t.Fatal("no evidence watermark, so the delivery credit job will hand the new address the old mailbox's record")
	}
	// esp_provider is a cache of the address domain and the scheduler only
	// fills it when empty, so a stale one routes ESP-matched sends forever.
	if st.esp != "" {
		t.Fatalf("esp_provider after the address change = %q, want empty", st.esp)
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

// The delivery-credit job re-derives 'delivered' evidence from every step ever
// sent. Without the watermark the rows the address change deletes come back on
// its next pass, and the new address inherits a verdict earned by the old one.
func TestLiveContactEmailChangeSurvivesTheDeliveryCreditJob(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	repo := NewContactRepostory(handle)
	evidence := NewVerificationEvidenceRepository(handle)
	ctx := context.Background()

	seq := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO sequences (id, campaign_id, organization_id, name, subject, body_plain, body_html)
	      VALUES ($1, $2, $3, 'Email 1', 'Hi', 'Body', 'Body')`, seq, f.campaign, f.org); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO campaign_contact_progress (campaign_id, contact_id, sequence_id, sent_at, dispatched_at)
	      VALUES ($1, $2, $3, NOW() - interval '30 days', NOW() - interval '30 days')`, f.campaign, f.contact, seq); err != nil {
		t.Fatalf("progress: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, sql := range []string{
			`DELETE FROM contact_verification_evidence WHERE contact_id IN (SELECT id FROM contacts WHERE organization_id = $1)`,
			`DELETE FROM campaign_contact_progress WHERE campaign_id IN (SELECT id FROM campaigns WHERE organization_id = $1)`,
			`DELETE FROM sequences WHERE organization_id = $1`,
		} {
			if _, err := pool.Exec(c, sql, f.org); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	// The old address earns its delivery.
	if _, err := evidence.CreditCleanDeliveries(ctx, time.Hour, 1000); err != nil {
		t.Fatalf("credit: %v", err)
	}
	var rows int
	count := func() int {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM contact_verification_evidence WHERE contact_id = $1`, f.contact).Scan(&rows); err != nil {
			t.Fatalf("count evidence: %v", err)
		}
		return rows
	}
	if count() != 1 {
		t.Fatalf("evidence after the credit pass = %d, want 1", rows)
	}

	next := "moved@acme.test"
	if _, xerr := repo.Update(ctx, f.mate.String(), f.contact.String(), f.org, &models.UpdateContact{Email: &next}); xerr != nil {
		t.Fatalf("update email: %v", xerr)
	}
	if count() != 0 {
		t.Fatalf("evidence right after the address change = %d, want 0", rows)
	}

	// The next pass must not hand it back.
	if _, err := evidence.CreditCleanDeliveries(ctx, time.Hour, 1000); err != nil {
		t.Fatalf("credit again: %v", err)
	}
	if count() != 0 {
		t.Fatalf("the credit job re-derived %d evidence rows for the new address from mail sent to the old one", rows)
	}

	// A send that was already on the bus when the address changed has its
	// sent_at stamped by the worker's result afterwards, so reading sent_at
	// alone would let a delivery to the OLD mailbox through the watermark.
	// dispatched_at is when it left, and that is what the credit reads.
	if _, err := pool.Exec(ctx, `UPDATE campaign_contact_progress SET sent_at = NOW() WHERE campaign_id = $1 AND contact_id = $2`,
		f.campaign, f.contact); err != nil {
		t.Fatalf("late stamp: %v", err)
	}
	if _, err := evidence.CreditCleanDeliveries(ctx, 0, 1000); err != nil {
		t.Fatalf("credit in-flight: %v", err)
	}
	if count() != 0 {
		t.Fatalf("a send dispatched before the address change was credited to the new address (%d rows)", rows)
	}

	// Nor may a late event about that step: a hard bounce for the typo lands
	// after the correction and would otherwise mark the new address invalid
	// and stop every send to it.
	step := models.Step(&f.campaign, &seq)
	if inserted, err := evidence.Record(ctx, f.contact, step, "bounced_recipient", "late-bounce", "550 no such user", time.Now()); err != nil || inserted {
		t.Fatalf("a bounce for the old address was recorded against the new one (inserted=%v, err=%v)", inserted, err)
	}
	if count() != 0 {
		t.Fatalf("evidence after the late bounce = %d, want 0", rows)
	}

	// A step dispatched after the correction is about this address, and an
	// observation that names no step at all is always kept.
	if _, err := pool.Exec(ctx, `UPDATE campaign_contact_progress SET dispatched_at = NOW(), sent_at = NOW() WHERE campaign_id = $1 AND contact_id = $2`,
		f.campaign, f.contact); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if inserted, err := evidence.Record(ctx, f.contact, step, "opened", "fresh", "", time.Now()); err != nil || !inserted {
		t.Fatalf("an observation about the new address was refused (inserted=%v, err=%v)", inserted, err)
	}
	if inserted, err := evidence.Record(ctx, f.contact, models.EvidenceStep{}, "replied", "no-step", "", time.Now()); err != nil || !inserted {
		t.Fatalf("an observation with no step was refused (inserted=%v, err=%v)", inserted, err)
	}
	if count() != 2 {
		t.Fatalf("evidence after the two allowed observations = %d, want 2", rows)
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

	// Creating one goes through the same normalizer, so the two paths cannot
	// disagree about what an address is: mail.ParseAddress accepts a display
	// name and the whole string used to be stored as the recipient.
	created, xerr := repo.Add(ctx, f.owner.String(), f.org, []models.AddContact{{
		FirstName: "Dana", Email: "  Dana Reyes <Dana@Created.Test> ",
	}})
	if xerr != nil || len(created) != 1 {
		t.Fatalf("add: %v", xerr)
	}
	if created[0].Email != "dana@created.test" {
		t.Fatalf("created contact email = %q, want dana@created.test", created[0].Email)
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
