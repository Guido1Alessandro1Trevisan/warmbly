package email

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Live cover for what a disconnect leaves behind. Skipped unless
// WARMBLY_TEST_DB is set; see worker_removal_live_test.go for the DSN.
//
// None of this is visible to a stub. Whether a foreign key cascades, whether a
// thread label survives its last message, and whether the sealed refresh token
// can still be read once the mailbox row is gone are all facts about the
// schema, and every one of them was wrong before migration 000158.

// erasureRow is what the queue recorded for the deleted mailbox.
type erasureRow struct {
	present      bool
	refreshToken string
	blobPrefix   string
	provider     string
}

func readErasure(t *testing.T, f *removalLiveFixture) erasureRow {
	t.Helper()
	var row erasureRow
	err := f.pool.QueryRow(context.Background(), `
		SELECT refresh_token, blob_prefix, provider
		  FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox).
		Scan(&row.refreshToken, &row.blobPrefix, &row.provider)
	if err != nil {
		return erasureRow{}
	}
	row.present = true
	return row
}

func cleanErasure(t *testing.T, f *removalLiveFixture) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(),
			`DELETE FROM mailbox_erasures WHERE email_account_id = $1`, f.mailbox); err != nil {
			t.Errorf("cleanup erasure: %v", err)
		}
	})
}

// The delete has to write down the erasure it cannot perform. Read after the
// mailbox is gone, which is the only time that matters: email_accounts_oauth
// cascades with the mailbox, so a token not copied here is a grant that stays
// live on the customer's Google account with nothing left pointing at it.
func TestLiveDeleteRecordsTheErasureItCannotPerform(t *testing.T) {
	f := newRemovalLiveFixture(t)
	cleanErasure(t, f)

	// A Gmail mailbox with a stored grant, which is the case that matters.
	exec(t, f, `UPDATE email_accounts SET provider = 'gmail' WHERE id = $1`, f.mailbox)
	exec(t, f, `INSERT INTO email_accounts_oauth (email_account_id, access_token, refresh_token, expires_at)
	            VALUES ($1, 'sealed-access', 'sealed-refresh', now() + interval '1 hour')`, f.mailbox)

	if xerr := f.svc.Delete(context.Background(), f.user.String(), f.mailbox.String()); xerr != nil {
		t.Fatalf("delete: %v", xerr)
	}
	if f.mailboxExists(t) {
		t.Fatal("the mailbox is still there")
	}

	row := readErasure(t, f)
	if !row.present {
		t.Fatal("nothing was queued, so the grant stays live at Google and the stored mail stays in the bucket")
	}
	if row.refreshToken != "sealed-refresh" {
		t.Errorf("refresh token = %q, want it copied off the mailbox before the row cascaded away", row.refreshToken)
	}
	if row.provider != "gmail" {
		t.Errorf("provider = %q, want gmail so the job knows where to send the revocation", row.provider)
	}
	// Must match what the worker writes bodies under, or the erasure walks an
	// empty prefix and reports success over a bucket full of somebody's mail.
	want := "users/" + f.user.String() + "/emails/" + f.mailbox.String() + "/"
	if row.blobPrefix != want {
		t.Errorf("blob prefix = %q, want %q", row.blobPrefix, want)
	}
}

// An SMTP/IMAP mailbox has no grant, and must still be queued: its stored mail
// is the same mail.
func TestLiveDeleteQueuesErasureForAMailboxWithNoGrant(t *testing.T) {
	f := newRemovalLiveFixture(t)
	cleanErasure(t, f)

	if xerr := f.svc.Delete(context.Background(), f.user.String(), f.mailbox.String()); xerr != nil {
		t.Fatalf("delete: %v", xerr)
	}
	row := readErasure(t, f)
	if !row.present {
		t.Fatal("an SMTP/IMAP mailbox was deleted with its stored mail left in the bucket and nothing recording it")
	}
	if row.refreshToken != "" {
		t.Errorf("refresh token = %q, want empty: there is no grant to revoke", row.refreshToken)
	}
}

// A delete that fails leaves nothing queued. Erasing the mail of a mailbox
// that still exists would be worse than not erasing at all.
func TestLiveAFailedDeleteQueuesNoErasure(t *testing.T) {
	f := newRemovalLiveFixture(t)
	cleanErasure(t, f)
	f.pub.removeErr = errBusDown

	if xerr := f.svc.Delete(context.Background(), f.user.String(), f.mailbox.String()); xerr == nil {
		t.Fatal("the delete was reported as succeeding")
	}
	if !f.mailboxExists(t) {
		t.Fatal("the mailbox went despite the failure")
	}
	if readErasure(t, f).present {
		t.Error("queued the erasure of a mailbox that still exists and is still syncing")
	}
}

// The nine tables that named a mailbox with no foreign key. Each row here
// outlived its mailbox forever before 000158, and two of them hold the message
// ids and subjects of mail that arrived in somebody's inbox.
func TestLiveDeleteTakesTheRowsThatHadNoForeignKey(t *testing.T) {
	f := newRemovalLiveFixture(t)
	cleanErasure(t, f)
	ctx := context.Background()

	exec(t, f, `INSERT INTO email_message_map (email_id, user_id, message_id, id)
	            VALUES ($1, $2, 'msg-1', $3)`, f.mailbox, f.user, uuid.New())
	exec(t, f, `INSERT INTO email_history_ids (email_id, user_id, history_id)
	            VALUES ($1, $2, 42)`, f.mailbox, f.user)
	exec(t, f, `INSERT INTO email_delta_links (email_id, user_id, folder, delta_link)
	            VALUES ($1, $2, 'inbox', 'https://graph.example/delta')`, f.mailbox, f.user)
	// A warmup receipt names both mailboxes, so it is inserted in both
	// directions against a partner that stays. One shared mailbox on both
	// columns would pass on either foreign key alone and prove neither.
	partner := uuid.New()
	exec(t, f, `INSERT INTO email_accounts (id, user_id, organization_id, email, name,
	                signature_plain, signature_html, provider, status, campaign_limit, min_wait_time)
	            VALUES ($1, $2, $3, $4, 'Partner', '', '', 'smtp_imap', 'active', 50, 600)`,
		partner, f.user, f.org, "partner-"+partner.String()[:8]+"@test.local")
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM email_accounts WHERE id = $1`, partner); err != nil {
			t.Errorf("cleanup partner: %v", err)
		}
	})
	exec(t, f, `INSERT INTO warmup_received (email_account_id, internal_id, message_id, sender_account_id)
	            VALUES ($1, $2, 'warm-in', $3)`, f.mailbox, uuid.New(), partner)
	exec(t, f, `INSERT INTO warmup_received (email_account_id, internal_id, message_id, sender_account_id)
	            VALUES ($1, $2, 'warm-out', $3)`, partner, uuid.New(), f.mailbox)
	exec(t, f, `INSERT INTO warmup_tampering_events (email_account_id, message_id, kind)
	            VALUES ($1, 'warm-1', 'deletion')`, f.mailbox)
	exec(t, f, `INSERT INTO warmup_pending_engagements (email_account_id, payload, fire_at)
	            VALUES ($1, '{}'::jsonb, now())`, f.mailbox)

	if xerr := f.svc.Delete(ctx, f.user.String(), f.mailbox.String()); xerr != nil {
		t.Fatalf("delete: %v", xerr)
	}

	for _, check := range []struct{ table, column string }{
		{"email_message_map", "email_id"},
		{"email_history_ids", "email_id"},
		{"email_delta_links", "email_id"},
		{"warmup_received", "email_account_id"},
		{"warmup_received", "sender_account_id"},
		{"warmup_tampering_events", "email_account_id"},
		{"warmup_pending_engagements", "email_account_id"},
	} {
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+check.table+` WHERE `+check.column+` = $1`, f.mailbox).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", check.table, err)
		}
		if n != 0 {
			t.Errorf("%s.%s kept %d rows for a mailbox that no longer exists", check.table, check.column, n)
		}
	}
}

// Thread labels are keyed by (organization, thread) and snoozes by (user,
// thread); neither names a mailbox, so nothing cascades them. Before this,
// disconnecting a mailbox left the workspace with labels and snoozes on threads
// that had no messages left, which the unibox counts and cannot open. A thread
// that also ran through a mailbox they kept must survive untouched.
func TestLiveDeleteClearsLabelsOnThreadsItEmptied(t *testing.T) {
	f := newRemovalLiveFixture(t)
	cleanErasure(t, f)
	ctx := context.Background()

	// A second mailbox the owner keeps, so the test can tell "clean up after
	// this mailbox" apart from "delete everything this person labelled".
	kept := uuid.New()
	exec(t, f, `INSERT INTO email_accounts (id, user_id, organization_id, email, name,
	                signature_plain, signature_html, provider, status, campaign_limit, min_wait_time)
	            VALUES ($1, $2, $3, $4, 'Kept', '', '', 'smtp_imap', 'active', 50, 600)`,
		kept, f.user, f.org, "kept-"+kept.String()[:8]+"@test.local")
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM email_accounts WHERE id = $1`, kept); err != nil {
			t.Errorf("cleanup kept mailbox: %v", err)
		}
	})

	category := uuid.New()
	exec(t, f, `INSERT INTO categories (id, user_id, organization_id, title, color, "position")
	            VALUES ($1, $2, $3, 'Live Erasure', '#3366cc', 1)`, category, f.user, f.org)

	doomed, survivor := "thread-doomed-"+f.mailbox.String()[:8], "thread-kept-"+kept.String()[:8]
	for _, m := range []struct {
		mailbox uuid.UUID
		thread  string
	}{{f.mailbox, doomed}, {kept, survivor}} {
		exec(t, f, `INSERT INTO unibox_emails (id, user_id, email_id, thread_id, message_id, subject)
		            VALUES ($1, $2, $3, $4, $5, 'live erasure')`,
			uuid.New(), f.user, m.mailbox, m.thread, "mid-"+m.thread)
		exec(t, f, `INSERT INTO unibox_thread_labels (organization_id, user_id, thread_id, category_id)
		            VALUES ($1, $2, $3, $4)`, f.org, f.user, m.thread, category)
		exec(t, f, `INSERT INTO unibox_snoozes (user_id, thread_id, snoozed_until) VALUES ($1, $2, now() + interval '1 day')`,
			f.user, m.thread)
	}

	if xerr := f.svc.Delete(ctx, f.user.String(), f.mailbox.String()); xerr != nil {
		t.Fatalf("delete: %v", xerr)
	}

	for _, tbl := range []string{"unibox_thread_labels", "unibox_snoozes"} {
		var gone, left int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+tbl+` WHERE user_id = $1 AND thread_id = $2`, f.user, doomed).Scan(&gone); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+tbl+` WHERE user_id = $1 AND thread_id = $2`, f.user, survivor).Scan(&left); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if gone != 0 {
			t.Errorf("%s kept %d rows on a thread with no messages left", tbl, gone)
		}
		if left != 1 {
			t.Errorf("%s dropped a row on a thread in a mailbox the owner kept (%d left)", tbl, left)
		}
	}
}

// exec runs one fixture statement.
func exec(t *testing.T, f *removalLiveFixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture %q: %v", sql, err)
	}
}
