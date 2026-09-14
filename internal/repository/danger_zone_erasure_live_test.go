package repository

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/infrastructure/db"
)

// Live cover for deleting a workspace and deleting a person. Skipped unless
// WARMBLY_TEST_DB is set:
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_dev?sslmode=disable \
//	  go test ./internal/repository/ -run Live -v
//
// Both paths were one DELETE against a parent row, documented as relying on
// cascades. Only a real schema can say whether those cascades exist, and for
// the workspace four of them did not.

type dangerLiveFixture struct {
	handle  *db.DB
	repo    DangerZoneRepository
	user    uuid.UUID
	org     uuid.UUID
	mailbox uuid.UUID
}

func newDangerLiveFixture(t *testing.T) *dangerLiveFixture {
	t.Helper()
	dsn := os.Getenv("WARMBLY_TEST_DB")
	if dsn == "" {
		t.Skip("WARMBLY_TEST_DB not set")
	}
	handle, err := db.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { handle.Pool.Close() })

	f := &dangerLiveFixture{
		handle:  handle,
		repo:    NewDangerZoneRepository(handle.Pool),
		user:    uuid.New(),
		org:     uuid.New(),
		mailbox: uuid.New(),
	}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := handle.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'Danger', 'Test')`,
		f.user, "danger-"+f.user.String()[:8]+"@test.local")
	exec(`INSERT INTO organizations (id, name, slug, owner_user_id) VALUES ($1, 'Danger Test', $2, $3)`,
		f.org, "danger-"+f.org.String()[:8], f.user)
	exec(`INSERT INTO email_accounts (id, user_id, organization_id, email, name,
	          signature_plain, signature_html, provider, status, campaign_limit, min_wait_time)
	      VALUES ($1, $2, $3, $4, 'Danger', '', '', 'gmail', 'active', 50, 600)`,
		f.mailbox, f.user, f.org, "danger-"+f.mailbox.String()[:8]+"@test.local")
	exec(`INSERT INTO email_accounts_oauth (email_account_id, access_token, refresh_token, expires_at)
	      VALUES ($1, 'sealed-access', 'sealed-refresh', now() + interval '1 hour')`, f.mailbox)

	t.Cleanup(func() {
		c := context.Background()
		for _, step := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox},
			{`DELETE FROM email_accounts WHERE id = $1`, f.mailbox},
			{`DELETE FROM organizations WHERE id = $1`, f.org},
			{`DELETE FROM users WHERE id = $1`, f.user},
		} {
			if _, err := handle.Pool.Exec(c, step.sql, step.arg); err != nil {
				t.Errorf("cleanup %q: %v", step.sql, err)
			}
		}
	})
	return f
}

func (f *dangerLiveFixture) count(t *testing.T, sql string, arg any) int {
	t.Helper()
	var n int
	if err := f.handle.Pool.QueryRow(context.Background(), sql, arg).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The defect: four foreign keys into organizations had no delete action, so
// deleting a workspace raised a foreign key violation for any workspace that
// had ever held a mailbox, a campaign, a contact or a sequence. A scheduled
// workspace deletion ran its grace period, mailed its warnings, and then failed
// every tick forever with the workspace still there.
func TestLiveDeletingAWorkspaceSucceeds(t *testing.T) {
	f := newDangerLiveFixture(t)

	if err := f.repo.HardDeleteOrganization(context.Background(), f.org); err != nil {
		t.Fatalf("a workspace with one mailbox could not be deleted: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.org); n != 0 {
		t.Error("the workspace is still there")
	}
	if n := f.count(t, `SELECT count(*) FROM email_accounts WHERE id = $1`, f.mailbox); n != 0 {
		t.Error("the workspace's mailbox outlived the workspace")
	}
}

// Deleting the workspace must hand its mailboxes' grants back too. Without
// this, a customer who closes their account leaves Warmbly connected to their
// Gmail with nothing left in the database that knows it.
func TestLiveDeletingAWorkspaceQueuesItsMailboxesForErasure(t *testing.T) {
	f := newDangerLiveFixture(t)

	if err := f.repo.HardDeleteOrganization(context.Background(), f.org); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	var token, prefix string
	err := f.handle.Pool.QueryRow(context.Background(),
		`SELECT refresh_token, blob_prefix FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox).
		Scan(&token, &prefix)
	if err != nil {
		t.Fatalf("nothing queued for the workspace's mailbox: %v", err)
	}
	if token != "sealed-refresh" {
		t.Errorf("refresh token = %q, want it copied off the mailbox before the cascade", token)
	}
	if prefix == "" {
		t.Error("no blob prefix recorded, so the workspace's mail stays in the bucket")
	}
}

// A workspace whose mailboxes are in a warmup pool with a standing on record.
// Deleting it has to unwind organizations -> email_accounts ->
// warmup_pool_participants alongside warmup_reputation_ledger, which hangs off
// the organization by its own foreign key. The other workspace tests use a bare
// mailbox and never build that state, so nothing else here would notice if one
// of those links stopped resolving; a foreign key that does not aborts the
// whole deletion rather than losing a row.
//
// The mirror trigger fires on insert and update, not delete, so the cascade
// does not run it. This covers the cascade and the ledger's own key, not the
// trigger's behaviour during a delete.
func TestLiveDeletingAWorkspaceWithWarmupStandingSucceeds(t *testing.T) {
	f := newDangerLiveFixture(t)
	ctx := context.Background()

	// The free pool, seeded under a fixed id on every instance.
	const freePool = "77777777-aaaa-0000-0000-000000000001"
	if _, err := f.handle.Pool.Exec(ctx, `
		INSERT INTO warmup_pool_participants (pool_id, email_account_id, health_state, blocked_at, blocked_until, blocked_reason)
		VALUES ($1, $2, 'blocked', now(), now() + interval '30 days', 'live test')`, freePool, f.mailbox); err != nil {
		t.Fatalf("fixture participant: %v", err)
	}

	// The mirror writes the standing against the address; assert it is there,
	// or the test would pass without having exercised anything.
	var mirrored int
	if err := f.handle.Pool.QueryRow(ctx,
		`SELECT count(*) FROM warmup_reputation_ledger WHERE organization_id = $1`, f.org).Scan(&mirrored); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if mirrored != 1 {
		t.Fatalf("the reputation mirror wrote %d rows, want 1: this test is not exercising the trigger", mirrored)
	}

	if err := f.repo.HardDeleteOrganization(ctx, f.org); err != nil {
		t.Fatalf("a workspace with a penalised mailbox could not be deleted: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM organizations WHERE id = $1`, f.org); n != 0 {
		t.Error("the workspace is still there")
	}
	if n := f.count(t, `SELECT count(*) FROM warmup_reputation_ledger WHERE organization_id = $1`, f.org); n != 0 {
		t.Error("the workspace's reputation ledger outlived the workspace")
	}
	// And the erasure is still queued: the warmup rows must not have displaced it.
	if n := f.count(t, `SELECT count(*) FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox); n != 1 {
		t.Errorf("queued %d erasures, want 1", n)
	}
}

// Closing an account cannot complete today for an unrelated reason: twenty
// foreign keys into users have no delete action, including
// scheduled_deletions.requested_by_user_id, which the deletion request itself
// writes. Repairing that is account-closure work, with a separate decision per
// constraint about what survives a person leaving, and it is not done here.
//
// What IS this change's business is that the failure stays clean. The enqueue
// shares the delete's transaction, so a delete that cannot happen must leave
// nothing queued: erasing a live mailbox's grant and its stored mail because a
// deletion half-ran would be far worse than the deletion not running.
func TestLiveAFailedAccountDeletionQueuesNoErasure(t *testing.T) {
	f := newDangerLiveFixture(t)
	ctx := context.Background()

	// The row the real flow writes, and the one that blocks the delete.
	if _, err := f.handle.Pool.Exec(ctx, `
		INSERT INTO scheduled_deletions (id, resource_type, resource_id, requested_by_user_id,
		    scheduled_at, execute_after, grace_days, status)
		VALUES ($1, 'user', $2, $2, now(), now(), 7, 'pending')`, uuid.New(), f.user); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.handle.Pool.Exec(context.Background(),
			`DELETE FROM scheduled_deletions WHERE resource_id = $1`, f.user); err != nil {
			t.Errorf("cleanup scheduled deletion: %v", err)
		}
	})

	if err := f.repo.HardDeleteUser(ctx, f.user); err == nil {
		t.Fatal("the account deletion reported success; if it now works, this test should assert the erasure instead")
	}
	if n := f.count(t, `SELECT count(*) FROM users WHERE id = $1`, f.user); n != 1 {
		t.Fatal("the user went after all")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox); n != 0 {
		t.Errorf("queued %d erasures for a mailbox that is still connected and still syncing", n)
	}
}
