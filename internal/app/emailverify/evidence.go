package emailverify

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/warmbly/warmbly/internal/config"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/emailverify"
	"github.com/warmbly/warmbly/internal/repository"
)

// EvidenceRecorder is what the send, tracking, reply and bounce paths call
// to teach verification what real mail showed. Every call is idempotent on
// (contact, kind, ref) and best effort: a failure is logged, never returned
// into the caller's path.
type EvidenceRecorder interface {
	RecordEvidence(ctx context.Context, contactID uuid.UUID, step models.EvidenceStep, kind, ref, detail string)
}

// Evidence scores contacts from the ledger. It is separate from Service so
// the consumer, which never verifies, can record and rescore without the
// verifier and its providers.
type Evidence struct {
	repo repository.VerificationEvidenceRepository
	// onChange runs after a rescore changed the contact's status, e.g. to
	// resume a campaign parked on the old verdict.
	onChange func(ctx context.Context, contactID uuid.UUID)
}

func NewEvidence(repo repository.VerificationEvidenceRepository) *Evidence {
	return &Evidence{repo: repo}
}

func (e *Evidence) SetOnChange(fn func(ctx context.Context, contactID uuid.UUID)) { e.onChange = fn }

func (e *Evidence) RecordEvidence(ctx context.Context, contactID uuid.UUID, step models.EvidenceStep, kind, ref, detail string) {
	if e == nil || e.repo == nil || contactID == uuid.Nil {
		return
	}
	inserted, err := e.repo.Record(ctx, contactID, step, kind, ref, detail, time.Now().UTC())
	if err != nil {
		log.Warn().Err(err).Str("contact_id", contactID.String()).Str("kind", kind).Msg("verification evidence not recorded")
		return
	}
	if inserted {
		e.Rescore(ctx, contactID)
	}
}

// Rescore recomputes the contact's status and confidence from its verdict
// and ledger, and persists the result.
func (e *Evidence) Rescore(ctx context.Context, contactID uuid.UUID) {
	if e == nil || e.repo == nil {
		return
	}
	verdict, err := e.repo.Verdict(ctx, contactID)
	if err != nil {
		return
	}
	rows, err := e.repo.ListForContact(ctx, contactID)
	if err != nil {
		return
	}
	scored := emailverify.Score(verdict, toEvidence(rows), time.Now().UTC())
	reason := ""
	if len(scored.Reasons) > 0 {
		reason = scored.Reasons[0]
	}
	if err := e.repo.SetScore(ctx, contactID, string(scored.Status), scored.Confidence, reason, scored.LastPositiveAt, scored.Decisive); err != nil {
		return
	}
	if scored.Decisive && scored.Status != verdict.Status && e.onChange != nil {
		e.onChange(ctx, contactID)
	}
}

// Explain builds the "why" a member reads on the contact drawer.
func (e *Evidence) Explain(ctx context.Context, contactID uuid.UUID) *models.ContactVerificationDetail {
	if e == nil || e.repo == nil {
		return nil
	}
	verdict, err := e.repo.Verdict(ctx, contactID)
	if err != nil {
		return nil
	}
	rows, err := e.repo.ListForContact(ctx, contactID)
	if err != nil {
		return nil
	}
	scored := emailverify.Score(verdict, toEvidence(rows), time.Now().UTC())
	return &models.ContactVerificationDetail{
		Status:     string(scored.Status),
		Confidence: scored.Confidence,
		Reasons:    scored.Reasons,
		Decisive:   scored.Decisive,
		Evidence:   rows,
	}
}

// CreditCleanDeliveries turns sends that never bounced into evidence, then
// rescores the contacts credited. Returns how many contacts changed.
func (e *Evidence) CreditCleanDeliveries(ctx context.Context, limit int) (int, error) {
	if e == nil || e.repo == nil {
		return 0, nil
	}
	ids, err := e.repo.CreditCleanDeliveries(ctx, time.Duration(config.VerificationDeliveryWindowHours)*time.Hour, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		e.Rescore(ctx, id)
	}
	return len(ids), nil
}

// Apply folds the ledger into a fresh check result before it is stored, so a
// probe can never demote an address real mail has proven.
func (e *Evidence) Apply(ctx context.Context, contactID uuid.UUID, res emailverify.Result) emailverify.Result {
	source := models.VerificationSourceProbe
	if res.Provider != "" && res.Provider != emailverify.ProviderBuiltin {
		source = models.VerificationSourceProvider
	}
	verdict := emailverify.Verdict{Status: res.Status, Source: source, CheckedAt: res.CheckedAt}
	var rows []models.ContactVerificationEvidence
	if e != nil && e.repo != nil {
		rows, _ = e.repo.ListForContact(ctx, contactID)
	}
	scored := emailverify.Score(verdict, toEvidence(rows), time.Now().UTC())
	res.Confidence = scored.Confidence
	if scored.Decisive && scored.Status != res.Status {
		res.Status = scored.Status
		if len(scored.Reasons) > 0 {
			res.Reason = scored.Reasons[0] + " (check said: " + res.Reason + ")"
		}
		if scored.Status == emailverify.StatusValid {
			res.SubStatus = emailverify.SubStatusNone
			res.IsCatchAll = false
		}
	}
	return res
}

func toEvidence(rows []models.ContactVerificationEvidence) []emailverify.Evidence {
	out := make([]emailverify.Evidence, 0, len(rows))
	for _, r := range rows {
		out = append(out, emailverify.Evidence{Kind: r.Kind, Detail: r.Detail, ObservedAt: r.ObservedAt})
	}
	return out
}
