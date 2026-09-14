package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/warmbly/warmbly/internal/config"
	"github.com/warmbly/warmbly/internal/infrastructure/db"
)

// MailboxErasure is the erasure still owed for a mailbox that is already gone:
// its OAuth grant handed back to the provider, and its message bodies removed
// from object storage. Neither can be done inside the transaction that deletes
// the mailbox (one is an HTTP call, the other an object store), and neither may
// be lost if the process handling the delete goes away, so they are written
// down and worked off.
type MailboxErasure struct {
	EmailAccountID uuid.UUID
	OrganizationID *uuid.UUID
	UserID         uuid.UUID
	Email          string
	Provider       string
	// RefreshToken is still sealed under CREDENTIALS_ENCRYPTION_KEY, exactly as
	// it was stored. Empty when the mailbox had no grant.
	RefreshToken   string
	BlobPrefix     string
	TokenRevokedAt *time.Time
	BlobsErasedAt  *time.Time
	Attempts       int
	LastError      string
}

// MailboxErasureRepository is the erasure queue.
type MailboxErasureRepository interface {
	// Claim leases up to limit due rows for one pass. A leased row is not
	// returned to another backend until the lease expires, so two instances
	// never revoke the same grant or walk the same prefix at once.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]MailboxErasure, error)
	// MarkTokenRevoked and MarkBlobsErased record half the work, so a failure
	// in the other half never repeats the one that succeeded. Revoking twice
	// is harmless; walking a large prefix again is not free, and a second
	// revoke of a token we no longer hold is impossible.
	MarkTokenRevoked(ctx context.Context, id uuid.UUID) error
	MarkBlobsErased(ctx context.Context, id uuid.UUID) error
	// Complete removes the row. The erasure is finished, and the row itself
	// holds the address of the mailbox the customer asked us to forget, so
	// keeping it as a receipt would be keeping the thing being erased. The
	// audit log records that the deletion happened.
	Complete(ctx context.Context, id uuid.UUID) error
	// Fail records why and when to try again.
	Fail(ctx context.Context, id uuid.UUID, cause string, retryAt time.Time) error
	// PendingOlderThan counts outstanding erasures older than d, which is the
	// number that should be zero and the one worth alerting on.
	PendingOlderThan(ctx context.Context, d time.Duration) (int, error)
}

type mailboxErasureRepository struct {
	DB *db.DB
}

func NewMailboxErasureRepository(database *db.DB) MailboxErasureRepository {
	return &mailboxErasureRepository{DB: database}
}

// erasureColumns is the row every read returns.
const erasureColumns = `email_account_id, organization_id, user_id, email, provider,
	refresh_token, blob_prefix, token_revoked_at, blobs_erased_at, attempts, last_error`

func scanErasure(rows pgx.Rows) (MailboxErasure, error) {
	var e MailboxErasure
	err := rows.Scan(&e.EmailAccountID, &e.OrganizationID, &e.UserID, &e.Email, &e.Provider,
		&e.RefreshToken, &e.BlobPrefix, &e.TokenRevokedAt, &e.BlobsErasedAt, &e.Attempts, &e.LastError)
	return e, err
}

func (r *mailboxErasureRepository) Claim(ctx context.Context, limit int, lease time.Duration) ([]MailboxErasure, error) {
	if limit <= 0 {
		limit = 50
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	// SKIP LOCKED is what makes this safe to run on every backend at once: a
	// row another instance is already holding is passed over rather than
	// waited on. Pushing next_attempt_at out by the lease means a process that
	// dies mid-erasure releases its rows by the clock instead of holding them.
	query := `
		UPDATE mailbox_erasures
		   SET next_attempt_at = now() + $2::interval
		 WHERE email_account_id IN (
		       SELECT email_account_id
		         FROM mailbox_erasures
		        WHERE next_attempt_at <= now()
		        ORDER BY next_attempt_at
		        LIMIT $1
		          FOR UPDATE SKIP LOCKED
		 )
		RETURNING ` + erasureColumns
	params := []any{limit, lease.String()}

	rows, err := r.DB.Query(ctx, query, params...)
	if err != nil {
		db.CaptureError(err, query, params, "query")
		return nil, err
	}
	defer rows.Close()

	var out []MailboxErasure
	for rows.Next() {
		e, err := scanErasure(rows)
		if err != nil {
			db.CaptureError(err, query, params, "scan")
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *mailboxErasureRepository) MarkTokenRevoked(ctx context.Context, id uuid.UUID) error {
	return r.stamp(ctx, id, "token_revoked_at")
}

func (r *mailboxErasureRepository) MarkBlobsErased(ctx context.Context, id uuid.UUID) error {
	return r.stamp(ctx, id, "blobs_erased_at")
}

// stamp records one half as done. column is never caller-supplied; the two
// call sites above are the whole set.
func (r *mailboxErasureRepository) stamp(ctx context.Context, id uuid.UUID, column string) error {
	query := fmt.Sprintf(`UPDATE mailbox_erasures SET %s = now() WHERE email_account_id = $1`, column)
	if _, err := r.DB.Exec(ctx, query, id); err != nil {
		db.CaptureError(err, query, []any{id}, "exec")
		return err
	}
	return nil
}

func (r *mailboxErasureRepository) Complete(ctx context.Context, id uuid.UUID) error {
	query := `DELETE FROM mailbox_erasures WHERE email_account_id = $1`
	if _, err := r.DB.Exec(ctx, query, id); err != nil {
		db.CaptureError(err, query, []any{id}, "exec")
		return err
	}
	return nil
}

func (r *mailboxErasureRepository) Fail(ctx context.Context, id uuid.UUID, cause string, retryAt time.Time) error {
	// Truncated, because a provider error body can be long and this column is
	// read by a human in the admin panel, not parsed. By runes, not bytes: a
	// provider message is arbitrary remote text, and cutting one mid-character
	// yields invalid UTF-8 that Postgres rejects outright, which would lose the
	// attempt count and the backoff along with the message.
	cause = truncateRunes(cause, 500)
	query := `
		UPDATE mailbox_erasures
		   SET attempts = attempts + 1, last_error = $2, next_attempt_at = $3
		 WHERE email_account_id = $1
	`
	params := []any{id, cause, retryAt}
	if _, err := r.DB.Exec(ctx, query, params...); err != nil {
		db.CaptureError(err, query, params, "exec")
		return err
	}
	return nil
}

func (r *mailboxErasureRepository) PendingOlderThan(ctx context.Context, d time.Duration) (int, error) {
	query := `SELECT count(*) FROM mailbox_erasures WHERE created_at < now() - $1::interval`
	var n int
	if err := r.DB.QueryRow(ctx, query, d.String()).Scan(&n); err != nil {
		db.CaptureError(err, query, []any{d.String()}, "queryrow")
		return 0, err
	}
	return n, nil
}

// EnqueueMailboxErasures records the erasure owed for every mailbox matching
// scope, inside the caller's transaction, and returns how many were recorded.
// It must be called BEFORE the delete that removes them: it reads the mailbox
// row and the sealed refresh token that cascades away with it.
//
// scope is a WHERE fragment over email_accounts, aliased `a`, written by this
// package's own callers and never by a request. Its placeholders start at $1.
// CollectMailboxThreadState takes the same fragment, so a caller writes one
// scope and hands it to both.
//
// The blob prefix is resolved here rather than in SQL so the layout has one
// definition (config.StorageEndpointMailboxPrefix), which is also what writes
// the bodies. A prefix built a second time in SQL would drift from it silently,
// and the failure would be message bodies left in the bucket with nothing
// pointing at them.
func EnqueueMailboxErasures(ctx context.Context, tx pgx.Tx, scope string, args ...any) (int, error) {
	find := `SELECT a.id, a.user_id FROM email_accounts a WHERE ` + scope
	rows, err := tx.Query(ctx, find, args...)
	if err != nil {
		db.CaptureError(err, find, args, "query")
		return 0, err
	}
	var ids []uuid.UUID
	var prefixes []string
	for rows.Next() {
		var id, userID uuid.UUID
		if err := rows.Scan(&id, &userID); err != nil {
			rows.Close()
			db.CaptureError(err, find, args, "scan")
			return 0, err
		}
		ids = append(ids, id)
		prefixes = append(prefixes, config.StorageEndpointMailboxPrefix(userID, id))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		db.CaptureError(err, find, args, "rows")
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// ON CONFLICT is belt and braces. No path reaches it today: every caller
	// joins email_accounts, and a mailbox already enqueued no longer has a row
	// to join to. It is here so that a future caller can never fail somebody's
	// delete on a duplicate key.
	//
	// It resets the progress stamps as well as the schedule. Keeping them would
	// mean a re-enqueued mailbox skipping the revocation because an earlier
	// erasure of a different grant had already been recorded as done.
	insert := `
		INSERT INTO mailbox_erasures
		    (email_account_id, organization_id, user_id, email, provider, refresh_token, blob_prefix)
		SELECT a.id, a.organization_id, a.user_id, a.email, a.provider::text,
		       COALESCE(o.refresh_token, ''), p.prefix
		  FROM unnest($1::uuid[], $2::text[]) AS p(id, prefix)
		  JOIN email_accounts a ON a.id = p.id
		  LEFT JOIN email_accounts_oauth o ON o.email_account_id = a.id
		ON CONFLICT (email_account_id) DO UPDATE
		   SET refresh_token    = EXCLUDED.refresh_token,
		       blob_prefix      = EXCLUDED.blob_prefix,
		       token_revoked_at = NULL,
		       blobs_erased_at  = NULL,
		       attempts         = 0,
		       last_error       = '',
		       next_attempt_at  = now()
	`
	tag, err := tx.Exec(ctx, insert, ids, prefixes)
	if err != nil {
		db.CaptureError(err, insert, []any{ids, prefixes}, "exec")
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ThreadState is the thread-keyed state one mailbox's owner may be left
// holding after that mailbox goes.
type ThreadState struct {
	OrganizationID *uuid.UUID
	UserID         uuid.UUID
	Threads        []string
}

// CollectMailboxThreadState lists the threads the mailboxes matching scope hold
// messages in, grouped by the workspace and person who could have labelled or
// snoozed them.
//
// It must run BEFORE the delete: the messages that name the threads cascade
// with the mailbox, so afterwards there is nothing left to ask.
//
// scope is the same fragment EnqueueMailboxErasures takes: a WHERE clause over
// email_accounts aliased `a`, written by this package's own callers.
func CollectMailboxThreadState(ctx context.Context, tx pgx.Tx, scope string, args ...any) ([]ThreadState, error) {
	query := `
		SELECT a.organization_id, a.user_id, array_agg(DISTINCT e.thread_id)
		  FROM email_accounts a
		  JOIN unibox_emails e ON e.email_id = a.id
		 WHERE (` + scope + `) AND e.thread_id <> ''
		 GROUP BY a.organization_id, a.user_id
	`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		db.CaptureError(err, query, args, "query")
		return nil, err
	}
	defer rows.Close()

	var out []ThreadState
	for rows.Next() {
		var st ThreadState
		if err := rows.Scan(&st.OrganizationID, &st.UserID, &st.Threads); err != nil {
			db.CaptureError(err, query, args, "scan")
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeleteOrphanedThreadState removes the labels and snoozes whose threads the
// delete just emptied.
//
// unibox_thread_labels is keyed by (organization_id, thread_id) and
// unibox_snoozes by (user_id, thread_id); neither names a mailbox, so nothing
// cascades them. Without this a disconnect leaves the workspace holding labels
// and snoozes on threads with no message left in them, which show up in the
// unibox's label counts as threads that cannot be opened.
//
// Runs AFTER the delete, in the same transaction, because the test is what is
// left in unibox_emails. Only the deleted mailbox's own threads are considered,
// and each is kept if any message in it survives somewhere the owner can still
// see: a thread that also ran through a mailbox they kept kepts its label.
func DeleteOrphanedThreadState(ctx context.Context, tx pgx.Tx, states []ThreadState) error {
	const labels = `
		DELETE FROM unibox_thread_labels l
		 WHERE l.organization_id = $1 AND l.thread_id = ANY($2)
		   AND NOT EXISTS (
		       SELECT 1 FROM unibox_emails e
		         JOIN email_accounts a ON a.id = e.email_id
		        WHERE a.organization_id = l.organization_id AND e.thread_id = l.thread_id)
	`
	const snoozes = `
		DELETE FROM unibox_snoozes s
		 WHERE s.user_id = $1 AND s.thread_id = ANY($2)
		   AND NOT EXISTS (
		       SELECT 1 FROM unibox_emails e
		        WHERE e.user_id = s.user_id AND e.thread_id = s.thread_id)
	`

	for _, st := range states {
		if len(st.Threads) == 0 {
			continue
		}
		// A mailbox with no workspace has no labels; snoozes are the owner's
		// either way.
		if st.OrganizationID != nil {
			if _, err := tx.Exec(ctx, labels, *st.OrganizationID, st.Threads); err != nil {
				db.CaptureError(err, labels, []any{*st.OrganizationID, st.Threads}, "exec")
				return err
			}
		}
		if _, err := tx.Exec(ctx, snoozes, st.UserID, st.Threads); err != nil {
			db.CaptureError(err, snoozes, []any{st.UserID, st.Threads}, "exec")
			return err
		}
	}
	return nil
}

// truncateRunes shortens s to at most max runes, never splitting a character.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.ToValidUTF8(s, ""))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max])
}
