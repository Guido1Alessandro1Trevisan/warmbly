package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/models"
)

// The advisor's pool finding explains itself with the band's own reason (#491),
// and that reason only reaches it through this thirty-column select. Nothing
// else proves the columns and the scan stay in step.
func TestLiveAdvisorSnapshotCarriesThePoolStanding(t *testing.T) {
	handle, pool := liveContactDB(t)
	ctx := context.Background()
	requireSchemaVersion(t, pool, 157)

	user, org, account := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture %q: %v", sql[:min(50, len(sql))], err)
		}
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM warmup_pool_participants WHERE email_account_id = $1`,
			`DELETE FROM email_accounts WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, sql, account); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if _, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, org); err != nil {
			t.Errorf("cleanup org: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	exec(`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'Advisor', 'Live')`,
		user, "adv-"+user.String()[:8]+"@test.local")
	exec(`INSERT INTO organizations (id, name, slug, owner_user_id) VALUES ($1, 'Advisor Live', $2, $3)`,
		org, "adv-"+org.String()[:8], user)
	exec(`INSERT INTO email_accounts (id, user_id, organization_id, email, name, signature_plain, signature_html,
	          provider, status, campaign_limit, min_wait_time, timezone)
	      VALUES ($1, $2, $3, $4, 'Advisor', '', '', 'smtp_imap', 'active', 50, 600, 'UTC')`,
		account, user, org, "adv-"+account.String()[:8]+"@test.local")
	exec(`INSERT INTO warmup_pool_participants (pool_id, email_account_id, health_state, last_health_score, last_health_reason, blocked_at, blocked_until)
	      VALUES ($1, $2, 'quarantined', 41.5, 'warmup spam placement 24.0% exceeded quarantine threshold', NOW(), NOW() + INTERVAL '7 days')`,
		models.WarmupPoolPremiumID, account)

	snap, err := NewAdvisorRepository(handle).LoadSnapshot(ctx, org, time.Now())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	var got *AdvisorMailbox
	for i := range snap.Mailboxes {
		if snap.Mailboxes[i].ID == account {
			got = &snap.Mailboxes[i]
		}
	}
	if got == nil {
		t.Fatal("the snapshot did not carry the mailbox at all")
	}
	if got.PoolHealth != "quarantined" || !got.PoolBlocked {
		t.Fatalf("pool standing = %q blocked=%v, want quarantined and blocked", got.PoolHealth, got.PoolBlocked)
	}
	if got.PoolHealthScore != 41.5 {
		t.Fatalf("pool health score = %v, want 41.5; the select and the scan have drifted", got.PoolHealthScore)
	}
	if got.PoolHealthReason != "warmup spam placement 24.0% exceeded quarantine threshold" {
		t.Fatalf("pool health reason = %q, want the band's own reason", got.PoolHealthReason)
	}
}
