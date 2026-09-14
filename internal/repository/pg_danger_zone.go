package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warmbly/warmbly/internal/models"
)

// DangerZoneRepository persists scheduled deletions and toggles the
// matching deletion timestamp on the parent table.
type DangerZoneRepository interface {
	// CreatePending inserts a new pending deletion and stamps the parent
	// resource's deletion_scheduled_* columns in the same transaction.
	// Returns ErrPendingDeletionExists if one already exists.
	CreatePending(ctx context.Context, d *models.ScheduledDeletion) error

	// Cancel marks a deletion as cancelled and clears the parent's
	// deletion timestamps. No-op if status != pending.
	Cancel(ctx context.Context, id uuid.UUID, cancelledByUserID uuid.UUID, reason string) error

	// GetActive returns the active (pending) deletion for a resource, if any.
	GetActive(ctx context.Context, rt models.DeletionResourceType, resourceID uuid.UUID) (*models.ScheduledDeletion, error)

	// GetByID returns any deletion by id.
	GetByID(ctx context.Context, id uuid.UUID) (*models.ScheduledDeletion, error)

	// ListDue returns pending deletions whose execute_after is <= now.
	ListDue(ctx context.Context, now time.Time, limit int) ([]models.ScheduledDeletion, error)

	// ListPendingForReminders returns pending deletions whose
	// execute_after is within the next windowEnd duration and that have
	// not yet had bit set in notifications_sent.
	ListPendingForReminders(ctx context.Context, within time.Duration, notSetBit int, limit int) ([]models.ScheduledDeletion, error)

	// MarkExecuting flips status from pending to executing (atomic guard).
	MarkExecuting(ctx context.Context, id uuid.UUID) (bool, error)

	// MarkCompleted records the finalized hard delete.
	MarkCompleted(ctx context.Context, id uuid.UUID) error

	// MarkFailed records an execution error so the next tick can retry.
	MarkFailed(ctx context.Context, id uuid.UUID, errMsg string) error

	// SetNotifBit OR's bit into notifications_sent (idempotent reminder gate).
	SetNotifBit(ctx context.Context, id uuid.UUID, bit int) error

	// HardDeleteOrganization removes the organization row, relying on
	// existing ON DELETE CASCADE FKs to clean up dependents.
	HardDeleteOrganization(ctx context.Context, orgID uuid.UUID) error

	// HardDeleteUser removes the user row.
	HardDeleteUser(ctx context.Context, userID uuid.UUID) error
}

// ErrPendingDeletionExists is returned when CreatePending is called for
// a resource that already has an active pending deletion.
var ErrPendingDeletionExists = errors.New("pending deletion already exists for this resource")

type dangerZoneRepository struct {
	db *pgxpool.Pool
}

// NewDangerZoneRepository constructs a DangerZoneRepository.
func NewDangerZoneRepository(db *pgxpool.Pool) DangerZoneRepository {
	return &dangerZoneRepository{db: db}
}

const scheduledDeletionColumns = `
	id, resource_type, resource_id, organization_id,
	requested_by_user_id, reason,
	scheduled_at, execute_after, grace_days, status,
	cancelled_at, cancelled_by_user_id, cancelled_reason,
	executed_at, execution_error,
	notifications_sent, last_reminder_at
`

func scanScheduledDeletion(row pgx.Row) (*models.ScheduledDeletion, error) {
	var d models.ScheduledDeletion
	var status string
	err := row.Scan(
		&d.ID, &d.ResourceType, &d.ResourceID, &d.OrganizationID,
		&d.RequestedByUserID, &d.Reason,
		&d.ScheduledAt, &d.ExecuteAfter, &d.GraceDays, &status,
		&d.CancelledAt, &d.CancelledByUserID, &d.CancelledReason,
		&d.ExecutedAt, &d.ExecutionError,
		&d.NotificationsSent, &d.LastReminderAt,
	)
	if err != nil {
		return nil, err
	}
	d.Status = models.DeletionStatus(status)
	return &d, nil
}

func (r *dangerZoneRepository) CreatePending(ctx context.Context, d *models.ScheduledDeletion) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	const insertQ = `
		INSERT INTO scheduled_deletions (
			id, resource_type, resource_id, organization_id,
			requested_by_user_id, reason,
			scheduled_at, execute_after, grace_days, status,
			notifications_sent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', 0)
	`

	_, err = tx.Exec(ctx, insertQ,
		d.ID, d.ResourceType, d.ResourceID, d.OrganizationID,
		d.RequestedByUserID, d.Reason,
		d.ScheduledAt, d.ExecuteAfter, d.GraceDays,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrPendingDeletionExists
		}
		return err
	}

	// Stamp the parent row so list endpoints can show "pending deletion"
	// without joining the scheduled_deletions table on every read.
	switch d.ResourceType {
	case models.DeletionResourceOrganization:
		_, err = tx.Exec(ctx,
			`UPDATE organizations
			   SET deletion_scheduled_at = $2,
			       deletion_scheduled_for = $3,
			       updated_at = NOW()
			 WHERE id = $1`,
			d.ResourceID, d.ScheduledAt, d.ExecuteAfter,
		)
	case models.DeletionResourceUser:
		_, err = tx.Exec(ctx,
			`UPDATE users
			   SET deletion_scheduled_at = $2,
			       deletion_scheduled_for = $3,
			       updated_at = NOW()
			 WHERE id = $1`,
			d.ResourceID, d.ScheduledAt, d.ExecuteAfter,
		)
	}
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *dangerZoneRepository) Cancel(ctx context.Context, id uuid.UUID, cancelledByUserID uuid.UUID, reason string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var resourceType string
	var resourceID uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE scheduled_deletions
		   SET status = 'cancelled',
		       cancelled_at = NOW(),
		       cancelled_by_user_id = $2,
		       cancelled_reason = NULLIF($3, '')
		 WHERE id = $1 AND status = 'pending'
		 RETURNING resource_type, resource_id
	`, id, cancelledByUserID, reason).Scan(&resourceType, &resourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not pending; nothing to do.
			return nil
		}
		return err
	}

	switch models.DeletionResourceType(resourceType) {
	case models.DeletionResourceOrganization:
		_, err = tx.Exec(ctx,
			`UPDATE organizations
			   SET deletion_scheduled_at = NULL,
			       deletion_scheduled_for = NULL,
			       updated_at = NOW()
			 WHERE id = $1`,
			resourceID,
		)
	case models.DeletionResourceUser:
		_, err = tx.Exec(ctx,
			`UPDATE users
			   SET deletion_scheduled_at = NULL,
			       deletion_scheduled_for = NULL,
			       updated_at = NOW()
			 WHERE id = $1`,
			resourceID,
		)
	}
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *dangerZoneRepository) GetActive(ctx context.Context, rt models.DeletionResourceType, resourceID uuid.UUID) (*models.ScheduledDeletion, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+scheduledDeletionColumns+`
		  FROM scheduled_deletions
		 WHERE resource_type = $1 AND resource_id = $2 AND status = 'pending'
		 LIMIT 1
	`, rt, resourceID)
	d, err := scanScheduledDeletion(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (r *dangerZoneRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.ScheduledDeletion, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+scheduledDeletionColumns+`
		  FROM scheduled_deletions WHERE id = $1
	`, id)
	d, err := scanScheduledDeletion(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (r *dangerZoneRepository) ListDue(ctx context.Context, now time.Time, limit int) ([]models.ScheduledDeletion, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+scheduledDeletionColumns+`
		  FROM scheduled_deletions
		 WHERE status = 'pending' AND execute_after <= $1
		 ORDER BY execute_after ASC
		 LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ScheduledDeletion
	for rows.Next() {
		d, err := scanScheduledDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

func (r *dangerZoneRepository) ListPendingForReminders(ctx context.Context, within time.Duration, notSetBit int, limit int) ([]models.ScheduledDeletion, error) {
	deadline := time.Now().Add(within)
	rows, err := r.db.Query(ctx, `
		SELECT `+scheduledDeletionColumns+`
		  FROM scheduled_deletions
		 WHERE status = 'pending'
		   AND execute_after > NOW()
		   AND execute_after <= $1
		   AND (notifications_sent & $2) = 0
		 ORDER BY execute_after ASC
		 LIMIT $3
	`, deadline, notSetBit, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ScheduledDeletion
	for rows.Next() {
		d, err := scanScheduledDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

func (r *dangerZoneRepository) MarkExecuting(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE scheduled_deletions
		   SET status = 'executing'
		 WHERE id = $1 AND status = 'pending'
	`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *dangerZoneRepository) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE scheduled_deletions
		   SET status = 'completed', executed_at = NOW(), execution_error = NULL
		 WHERE id = $1
	`, id)
	return err
}

func (r *dangerZoneRepository) MarkFailed(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE scheduled_deletions
		   SET status = 'pending',
		       execution_error = $2
		 WHERE id = $1
	`, id, errMsg)
	return err
}

func (r *dangerZoneRepository) SetNotifBit(ctx context.Context, id uuid.UUID, bit int) error {
	_, err := r.db.Exec(ctx, `
		UPDATE scheduled_deletions
		   SET notifications_sent = notifications_sent | $2,
		       last_reminder_at = NOW()
		 WHERE id = $1
	`, id, bit)
	return err
}

func (r *dangerZoneRepository) HardDeleteOrganization(ctx context.Context, orgID uuid.UUID) error {
	return r.hardDelete(ctx,
		`a.organization_id = $1`,
		`DELETE FROM organizations WHERE id = $1`,
		orgID)
}

func (r *dangerZoneRepository) HardDeleteUser(ctx context.Context, userID uuid.UUID) error {
	return r.hardDelete(ctx,
		`a.user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
		userID)
}

// hardDelete removes a workspace or a person and everything the cascades take
// with them, having first written down the erasure that the cascades cannot do.
//
// Deleting the rows is not deleting the data. Every mailbox in scope has an
// OAuth grant still live at its provider and message bodies still in object
// storage, and both references disappear with the rows, so the enqueue has to
// happen inside this transaction and before the delete. Without it, a customer
// who closes their account leaves Warmbly connected to their Gmail.
//
// mailboxScope selects those mailboxes as a WHERE clause over email_accounts
// aliased `a`; it is written here, never by a caller.
func (r *dangerZoneRepository) hardDelete(ctx context.Context, mailboxScope, deleteStmt string, id uuid.UUID) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := EnqueueMailboxErasures(ctx, tx, mailboxScope, id); err != nil {
		return err
	}

	// The threads those mailboxes hold, read before the rows go. Deleting a
	// workspace does not delete its members, so their labels and snoozes
	// outlive the messages they were attached to; deleting a person takes both
	// with them and this finds nothing to do.
	threads, err := CollectMailboxThreadState(ctx, tx, mailboxScope, id)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, deleteStmt, id); err != nil {
		return err
	}

	if err := DeleteOrphanedThreadState(ctx, tx, threads); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isUniqueViolation detects Postgres SQLSTATE 23505 (unique_violation).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	type pgErr interface {
		SQLState() string
	}
	if pe, ok := err.(pgErr); ok {
		return pe.SQLState() == "23505"
	}
	return false
}
