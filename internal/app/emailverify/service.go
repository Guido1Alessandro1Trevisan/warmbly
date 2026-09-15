// Package emailverify (app layer) orchestrates pre-send email verification:
// it loads contacts due for a check, picks the verifier for their organization
// (the paid provider connected on the workspace, else the in-house probe),
// persists each verdict, and offers the member-facing actions (re-verify,
// mark deliverable). The SMTP probe inside the in-house verifier dials remote
// MX hosts on :25 and must never run from a worker (a sending IP).
package emailverify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/warmbly/warmbly/internal/config"
	"github.com/warmbly/warmbly/internal/errx"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/emailverify"
	"github.com/warmbly/warmbly/internal/repository"
)

// Provider is a paid verification backend bound to one organization.
type Provider struct {
	Name string
	// Label is the provider's display name, for copy a member reads.
	Label string
	// ConnectionID is the integration connection carrying the key; nil for
	// the instance-wide key an operator configured.
	ConnectionID *uuid.UUID
	Client       emailverify.ProviderClient
}

// ProviderSource resolves the paid provider an organization connected.
// Implemented by the integration service.
type ProviderSource interface {
	// VerificationProviderFor returns nil when the org has no usable provider.
	VerificationProviderFor(ctx context.Context, orgID uuid.UUID) (*Provider, error)
	// ReportVerificationProviderError flips the connection's health when the
	// provider rejected the key or ran out of credits.
	ReportVerificationProviderError(ctx context.Context, connectionID uuid.UUID, err error)
	// ClearVerificationProviderError withdraws that error once the provider works.
	ClearVerificationProviderError(ctx context.Context, connectionID uuid.UUID)
}

// Service verifies contact email addresses before they are ever sent to.
type Service interface {
	// VerifyAddress verifies an arbitrary address for an organization without
	// touching the DB, through whichever verifier the org uses.
	VerifyAddress(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result

	// VerifyPending verifies up to `limit` contacts due for a check. Returns
	// the number processed. Driven by the scheduler.
	VerifyPending(ctx context.Context, limit int) (int, *errx.Error)

	// Request applies a member action: queue a re-check, or record a manual
	// verdict.
	Request(ctx context.Context, orgID uuid.UUID, req models.ContactVerificationRequest) (*models.ContactVerificationResponse, *errx.Error)

	// Overview reports which verifier the org uses, its credits, and the
	// org's contacts by verdict.
	Overview(ctx context.Context, orgID uuid.UUID) (*models.VerificationOverview, *errx.Error)

	// Kick wakes the scheduler for an immediate pass (after a re-verify
	// request or an import), instead of waiting for the next interval.
	Kick()
	// Wake is the channel the scheduler selects on.
	Wake() <-chan struct{}

	// SetVerdictHook registers a callback run once per organization after a
	// pass that changed its contacts, e.g. to resume a campaign that was
	// parked waiting for verification.
	SetVerdictHook(fn func(ctx context.Context, orgID uuid.UUID))

	// SetEvidence attaches the evidence ledger, so a check never overrides
	// what real mail has shown.
	SetEvidence(e *Evidence)
	// Explain builds the contact's verification detail for the drawer.
	Explain(ctx context.Context, contactID uuid.UUID) *models.ContactVerificationDetail
}

type service struct {
	repo      repository.ContactRepository
	builtin   emailverify.Verifier
	providers ProviderSource
	// platform is the instance-wide paid client an operator configured for
	// workspaces that bring no key of their own; nil when unset.
	platform     *Provider
	strict       bool
	builtinReady bool

	wake chan struct{}
	hook func(ctx context.Context, orgID uuid.UUID)

	evidence *Evidence

	// breakers are per organization: one tenant's list must not decide
	// whether another tenant's rejections are trusted.
	breakersMu sync.Mutex
	breakers   map[uuid.UUID]*breaker

	creditsMu sync.Mutex
	credits   map[string]creditsEntry

	// healthLocks serialize the connection-health writes for one provider, so
	// a report and a withdrawal raised by concurrent checks cannot reach the
	// database in the opposite order to the checks that raised them.
	healthMu    sync.Mutex
	healthLocks map[string]*sync.Mutex
}

type creditsEntry struct {
	n   *int
	err error
	// at is when this state was observed; until is when it stops being
	// believed. They differ because an exhausted account that the provider
	// cannot report a balance for is held longer than a routine lookup.
	at    time.Time
	until time.Time
}

// Options configures the service.
type Options struct {
	// Builtin is the in-house verifier. Required.
	Builtin emailverify.Verifier
	// BuiltinReady reports whether Builtin can run its SMTP probe (a public
	// HELO host is configured). Shown to members so a self-hosted instance
	// without one knows why every verdict is "unknown".
	BuiltinReady bool
	// Providers resolves per-org paid providers. Optional.
	Providers ProviderSource
	// PlatformMillionVerifierKey is the operator's own key, used for every
	// workspace without a key of its own. Optional.
	PlatformMillionVerifierKey string
	// PlatformBouncerKey is the operator's instance-wide Bouncer key.
	PlatformBouncerKey string
	// Strict refuses built-in fallback when the paid provider is unavailable.
	Strict bool
}

// NewService wires the verification service.
func NewService(repo repository.ContactRepository, opts Options) Service {
	s := &service{
		repo:         repo,
		builtin:      opts.Builtin,
		builtinReady: opts.BuiltinReady,
		providers:    opts.Providers,
		wake:         make(chan struct{}, 1),
		breakers:     map[uuid.UUID]*breaker{},
		credits:      map[string]creditsEntry{},
		healthLocks:  map[string]*sync.Mutex{},
		strict:       opts.Strict,
	}
	if k := strings.TrimSpace(opts.PlatformBouncerKey); k != "" {
		s.platform = &Provider{Name: emailverify.ProviderBouncer, Label: "Bouncer", Client: emailverify.NewBouncer(k, "")}
	} else if k := strings.TrimSpace(opts.PlatformMillionVerifierKey); k != "" {
		s.platform = &Provider{Name: emailverify.ProviderMillionVerifier, Label: "MillionVerifier", Client: emailverify.NewMillionVerifier(k, "")}
	}
	return s
}

func (s *service) Kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *service) Wake() <-chan struct{} { return s.wake }

func (s *service) SetVerdictHook(fn func(ctx context.Context, orgID uuid.UUID)) { s.hook = fn }

func (s *service) SetEvidence(e *Evidence) { s.evidence = e }

func (s *service) Explain(ctx context.Context, contactID uuid.UUID) *models.ContactVerificationDetail {
	return s.evidence.Explain(ctx, contactID)
}

func (s *service) breakerFor(orgID uuid.UUID) *breaker {
	s.breakersMu.Lock()
	defer s.breakersMu.Unlock()
	b, ok := s.breakers[orgID]
	if !ok {
		b = newBreaker(config.VerificationBreakerWindow, config.VerificationBreakerInvalidPct, time.Duration(config.VerificationBreakerCooldownMinutes)*time.Minute)
		s.breakers[orgID] = b
	}
	return b
}

// providerFor resolves the org's paid provider: its own connection first,
// then the operator's instance-wide key.
func (s *service) providerFor(ctx context.Context, orgID uuid.UUID) *Provider {
	if s.strict && s.platform != nil {
		return s.platform
	}
	if s.providers != nil {
		p, err := s.providers.VerificationProviderFor(ctx, orgID)
		if err != nil {
			log.Warn().Err(err).Str("organization_id", orgID.String()).Msg("verification: could not resolve provider; using built-in")
		} else if p != nil {
			return p
		}
	}
	if s.platform != nil {
		return s.platform
	}
	return nil
}

func (s *service) VerifyAddress(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result {
	if p := s.providerFor(ctx, orgID); p != nil {
		startedAt := time.Now()
		res, err := p.Client.Check(ctx, email)
		if err == nil {
			s.noteProviderOK(ctx, p, startedAt)
			return res
		}
		s.noteProviderError(ctx, p, err)
		if s.strict {
			return res
		}
	}
	return s.verifyBuiltin(ctx, orgID, email)
}

// verifyBuiltin runs the in-house verifier under the org's self-check breaker.
func (s *service) verifyBuiltin(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result {
	res := s.builtin.Verify(ctx, email)
	// Syntax, no-MX and disposable verdicts do not come from a probe, so they
	// neither feed nor fall under the breaker.
	probeVerdict := res.SubStatus == emailverify.SubStatusNone || res.SubStatus == emailverify.SubStatusCatchAll || res.SubStatus == emailverify.SubStatusRole
	if !probeVerdict {
		return res
	}
	if s.breakerFor(orgID).observe(res.Status == emailverify.StatusInvalid) && res.Status == emailverify.StatusInvalid {
		res.Status = emailverify.StatusUnknown
		res.Reason = "probe rejection not trusted: the in-house check is rejecting an unusual share of addresses and is cooling down (" + res.Reason + ")"
	}
	return res
}

// healthGate is the lock guarding one provider's connection health.
func (s *service) healthGate(p *Provider) *sync.Mutex {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	key := cacheKey(p)
	m, ok := s.healthLocks[key]
	if !ok {
		m = &sync.Mutex{}
		s.healthLocks[key] = m
	}
	return m
}

func (s *service) noteProviderError(ctx context.Context, p *Provider, err error) {
	if p == nil || err == nil {
		return
	}
	if errors.Is(err, emailverify.ErrProviderKey) || errors.Is(err, emailverify.ErrProviderCredits) {
		g := s.healthGate(p)
		g.Lock()
		defer g.Unlock()
		// Recorded before the connection is marked, so a check finishing
		// alongside this one compares itself against a failure that exists.
		s.cacheProviderState(p, nil, err)
		if p.ConnectionID != nil && s.providers != nil {
			s.providers.ReportVerificationProviderError(ctx, *p.ConnectionID, err)
		}
	}
}

// cacheProviderState records what is known about a provider's account. An
// exhausted account whose balance the provider never reports is held for a
// cooldown rather than a minute: nothing but a real check can retire it, so a
// shorter hold just re-runs the batch that discovered it.
func (s *service) cacheProviderState(p *Provider, n *int, err error) {
	ttl := time.Minute
	if errors.Is(err, emailverify.ErrProviderCredits) && !p.Client.ObservesBalance() {
		ttl = time.Duration(config.VerificationExhaustedCooldownMinutes) * time.Minute
	}
	now := time.Now()
	s.creditsMu.Lock()
	s.credits[cacheKey(p)] = creditsEntry{n: n, err: err, at: now, until: now.Add(ttl)}
	s.creditsMu.Unlock()
}

// providerDown reports an error recorded against the provider that has not
// lapsed, so a key that dies mid-pass stops the rest of the batch instead of
// burning a doomed call on every remaining address.
func (s *service) providerDown(p *Provider) bool {
	s.creditsMu.Lock()
	defer s.creditsMu.Unlock()
	e, ok := s.credits[cacheKey(p)]
	return ok && e.err != nil && time.Now().Before(e.until)
}

func cacheKey(p *Provider) string {
	if p.ConnectionID != nil {
		return p.ConnectionID.String()
	}
	return "platform"
}

// providerUsable caches account health and any available balance for a minute.
func (s *service) providerUsable(ctx context.Context, p *Provider) (*int, error) {
	key := cacheKey(p)
	s.creditsMu.Lock()
	e, ok := s.credits[key]
	s.creditsMu.Unlock()
	if ok && time.Now().Before(e.until) {
		return e.n, e.err
	}
	startedAt := time.Now()
	n, err := p.Client.Account(ctx)
	if err == nil && n != nil && *n <= 0 {
		err = emailverify.ErrProviderCredits
	}
	s.cacheProviderState(p, n, err)
	if err != nil {
		s.noteProviderError(ctx, p, err)
	} else if n != nil {
		// Only a reported balance withdraws a recorded failure. A provider that
		// answers without one has said the key works, never that there is
		// allowance left, so an observed 402 has to survive it.
		s.noteProviderOK(ctx, p, startedAt)
	}
	return n, err
}

// noteProviderOK withdraws a recorded provider error once something that began
// after it succeeds. Checks run concurrently and resolve out of order, so one
// that started before the failure must not clear it: it never saw it.
func (s *service) noteProviderOK(ctx context.Context, p *Provider, startedAt time.Time) {
	if p == nil {
		return
	}
	g := s.healthGate(p)
	g.Lock()
	defer g.Unlock()
	s.creditsMu.Lock()
	e, ok := s.credits[cacheKey(p)]
	newer := ok && e.err != nil && e.at.After(startedAt)
	if ok && e.err != nil && !newer {
		delete(s.credits, cacheKey(p))
	}
	s.creditsMu.Unlock()
	if newer || p.ConnectionID == nil || s.providers == nil {
		return
	}
	// The write is guarded in the repository, so a pass that finds the
	// connection already healthy costs nothing.
	s.providers.ClearVerificationProviderError(ctx, *p.ConnectionID)
}

func (s *service) VerifyPending(ctx context.Context, limit int) (int, *errx.Error) {
	cands, xerr := s.repo.ListVerificationCandidates(ctx, limit)
	if xerr != nil {
		return 0, xerr
	}
	if len(cands) == 0 {
		return 0, nil
	}

	// Group by organization: the verifier is chosen once per org.
	byOrg := map[uuid.UUID][]repository.VerificationCandidate{}
	var order []uuid.UUID
	for _, c := range cands {
		if _, seen := byOrg[c.OrganizationID]; !seen {
			order = append(order, c.OrganizationID)
		}
		byOrg[c.OrganizationID] = append(byOrg[c.OrganizationID], c)
	}

	processed := 0
	for _, orgID := range order {
		if err := ctx.Err(); err != nil {
			break
		}
		n := s.verifyOrgBatch(ctx, orgID, byOrg[orgID])
		processed += n
		if n > 0 && s.hook != nil {
			s.hook(ctx, orgID)
		}
	}
	return processed, nil
}

// verifyOrgBatch checks one org's candidates with its verifier, in parallel
// up to the verifier's concurrency, and persists every verdict.
func (s *service) verifyOrgBatch(ctx context.Context, orgID uuid.UUID, cands []repository.VerificationCandidate) int {
	verify := func(ctx context.Context, email string) emailverify.Result { return s.verifyBuiltin(ctx, orgID, email) }
	workers := config.VerificationProbeConcurrency
	if p := s.providerFor(ctx, orgID); p != nil {
		if _, err := s.providerUsable(ctx, p); err != nil {
			message := "verification: paid provider unusable; using built-in check"
			if s.strict {
				message = "verification: paid provider unusable; strict mode is holding recipients"
			}
			log.Warn().Err(err).Str("organization_id", orgID.String()).Msg(message)
			if s.strict {
				verify = func(context.Context, string) emailverify.Result {
					return emailverify.Result{Status: emailverify.StatusUnknown, Provider: p.Name, CheckedAt: time.Now().UTC(), Reason: p.Label + " verification unavailable"}
				}
			}
		} else {
			workers = config.VerificationProviderConcurrency
			verify = func(ctx context.Context, email string) emailverify.Result {
				if s.providerDown(p) {
					if s.strict {
						return emailverify.Result{Email: email, Status: emailverify.StatusUnknown, Provider: p.Name, CheckedAt: time.Now().UTC(), Reason: p.Label + " verification unavailable"}
					}
					return s.verifyBuiltin(ctx, orgID, email)
				}
				startedAt := time.Now()
				res, err := p.Client.Check(ctx, email)
				if err != nil {
					s.noteProviderError(ctx, p, err)
					if s.strict {
						return res
					}
					// Fall back for this address so the pass still makes progress.
					return s.verifyBuiltin(ctx, orgID, email)
				}
				s.noteProviderOK(ctx, p, startedAt)
				return res
			}
		}
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
		sem       = make(chan struct{}, workers)
	)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(c repository.VerificationCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			// What real mail showed outranks what the check says.
			res := s.evidence.Apply(ctx, c.ID, verify(ctx, c.Email))
			if xerr := s.repo.UpdateContactVerification(ctx, c.ID, res); xerr != nil {
				// Skip this one; a transient DB error shouldn't abort the whole pass.
				return
			}
			mu.Lock()
			processed++
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return processed
}

func (s *service) Request(ctx context.Context, orgID uuid.UUID, req models.ContactVerificationRequest) (*models.ContactVerificationResponse, *errx.Error) {
	var ids []uuid.UUID
	for _, raw := range req.Contacts {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, errx.ErrUuid
		}
		ids = append(ids, id)
	}
	if req.CampaignID != "" {
		cid, err := uuid.Parse(strings.TrimSpace(req.CampaignID))
		if err != nil {
			return nil, errx.ErrUuid
		}
		more, xerr := s.repo.UndeliverableLeadIDs(ctx, orgID, cid)
		if xerr != nil {
			return nil, xerr
		}
		ids = append(ids, more...)
	}
	if len(ids) == 0 {
		return nil, errx.NewWithIdentifier(errx.BadRequest, "no_contacts", "no contacts selected")
	}

	resp := &models.ContactVerificationResponse{Action: req.Action}
	switch req.Action {
	case models.ContactVerificationActionVerify:
		n, xerr := s.repo.ResetContactsVerification(ctx, orgID, ids)
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected, resp.Queued = n, true
		s.Kick()
	case models.ContactVerificationActionMarkDeliverable:
		n, xerr := s.repo.SetContactsVerification(ctx, orgID, ids, models.ContactVerificationWrite{
			Status:   string(emailverify.StatusValid),
			Reason:   "marked deliverable by a member",
			Provider: "manual",
			Source:   models.VerificationSourceManual,
		})
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected = n
		if s.hook != nil {
			s.hook(ctx, orgID)
		}
	case models.ContactVerificationActionMarkUndeliverable:
		n, xerr := s.repo.SetContactsVerification(ctx, orgID, ids, models.ContactVerificationWrite{
			Status:   string(emailverify.StatusInvalid),
			Reason:   "marked undeliverable by a member",
			Provider: "manual",
			Source:   models.VerificationSourceManual,
		})
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected = n
	default:
		return nil, errx.NewWithIdentifier(errx.BadRequest, "invalid_action", "action must be verify, mark_deliverable or mark_undeliverable")
	}
	return resp, nil
}

func (s *service) Overview(ctx context.Context, orgID uuid.UUID) (*models.VerificationOverview, *errx.Error) {
	counts, xerr := s.repo.VerificationCounts(ctx, orgID)
	if xerr != nil {
		return nil, xerr
	}
	out := &models.VerificationOverview{
		Provider:     emailverify.ProviderBuiltin,
		BuiltinReady: s.builtinReady,
		Counts:       counts,
	}
	if p := s.providerFor(ctx, orgID); p != nil {
		if p.ConnectionID != nil {
			id := p.ConnectionID.String()
			out.ConnectionID = &id
		}
		n, err := s.providerUsable(ctx, p)
		if err != nil {
			out.ProviderError = providerErrorText(p.Label, err)
		} else {
			out.Provider = p.Name
			out.Credits = n
		}
	}
	return out, nil
}

func providerErrorText(label string, err error) string {
	if label == "" {
		label = "The verification service"
	}
	switch {
	case errors.Is(err, emailverify.ErrProviderUnconfirmed):
		return label + " has not had its account email confirmed yet. Confirm it, then reconnect."
	case errors.Is(err, emailverify.ErrProviderKey):
		return label + " rejected the API key. Reconnect it with a current key."
	case errors.Is(err, emailverify.ErrProviderCredits):
		return "The " + label + " account has no allowance or credits left. Top it up to keep using it; the built-in check is used meanwhile."
	default:
		return label + " could not be reached; the built-in check is used meanwhile."
	}
}
