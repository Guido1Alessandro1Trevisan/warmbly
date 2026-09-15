package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/emailverify"
)

// A clean delivery is credited once per step, a bounce naming the recipient
// is recorded, and the score lands on the contact row.
func TestLiveVerificationEvidenceLedger(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	ctx := context.Background()
	repo := NewVerificationEvidenceRepository(handle)

	contact := addLead(t, f, "live"+uuid.New().String()[:6]+"@test.local", "invalid", true)
	seq := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO sequences (id, campaign_id, organization_id, name, subject, body_plain, body_html, position, kind) VALUES ($1, $2, $3, 'Step 1', 's', 'b', 'b', 1, 'email')`, seq, f.campaign, f.org); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO campaign_contact_progress (campaign_id, contact_id, sequence_id, sent_at) VALUES ($1, $2, $3, NOW() - interval '4 days')`, f.campaign, contact, seq); err != nil {
		t.Fatalf("progress: %v", err)
	}

	// The shared dev database may hold a backlog of older sends, so drain
	// until our step is reached.
	found := false
	for i := 0; i < 50 && !found; i++ {
		ids, err := repo.CreditCleanDeliveries(ctx, 72*time.Hour, 500)
		if err != nil {
			t.Fatalf("credit: %v", err)
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if id == contact {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("delivery not credited")
	}
	again, err := repo.CreditCleanDeliveries(ctx, 72*time.Hour, 100)
	if err != nil {
		t.Fatalf("credit again: %v", err)
	}
	for _, id := range again {
		if id == contact {
			t.Fatal("delivery credited twice")
		}
	}

	inserted, err := repo.Record(ctx, contact, models.EvidenceStep{}, emailverify.EvidenceReplied, "msg-1", "", time.Now())
	if err != nil || !inserted {
		t.Fatalf("record: %v %v", inserted, err)
	}
	if dup, _ := repo.Record(ctx, contact, models.EvidenceStep{}, emailverify.EvidenceReplied, "msg-1", "", time.Now()); dup {
		t.Fatal("same reply recorded twice")
	}
	rows, err := repo.ListForContact(ctx, contact)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %d %v", len(rows), err)
	}

	verdict, err := repo.Verdict(ctx, contact)
	if err != nil || verdict.Status != emailverify.StatusInvalid {
		t.Fatalf("verdict: %+v %v", verdict, err)
	}
	if err := repo.SetScore(ctx, contact, "valid", 91, "replied today", time.Now(), true); err != nil {
		t.Fatalf("set score: %v", err)
	}
	var status string
	var confidence int
	if err := pool.QueryRow(ctx, `SELECT verification_status, verification_confidence FROM contacts WHERE id = $1`, contact).Scan(&status, &confidence); err != nil {
		t.Fatal(err)
	}
	if status != "valid" || confidence != 91 {
		t.Fatalf("score not applied: %s %d", status, confidence)
	}
}
