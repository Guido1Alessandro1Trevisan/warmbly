package jobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/infrastructure/storage"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/encrypt"
	"github.com/warmbly/warmbly/internal/pkg/oauthrevoke"
	"github.com/warmbly/warmbly/internal/repository"
)

// stubErasureRepo is the queue, in memory, recording what the job did to it.
type stubErasureRepo struct {
	due []repository.MailboxErasure

	revoked    []uuid.UUID
	erased     []uuid.UUID
	completed  []uuid.UUID
	failed     []stubFailure
	pending    int
	pendingErr error
}

type stubFailure struct {
	id      uuid.UUID
	cause   string
	retryAt time.Time
}

func (s *stubErasureRepo) Claim(context.Context, int, time.Duration) ([]repository.MailboxErasure, error) {
	out := s.due
	s.due = nil
	return out, nil
}
func (s *stubErasureRepo) MarkTokenRevoked(_ context.Context, id uuid.UUID) error {
	s.revoked = append(s.revoked, id)
	return nil
}
func (s *stubErasureRepo) MarkBlobsErased(_ context.Context, id uuid.UUID) error {
	s.erased = append(s.erased, id)
	return nil
}
func (s *stubErasureRepo) Complete(_ context.Context, id uuid.UUID) error {
	s.completed = append(s.completed, id)
	return nil
}
func (s *stubErasureRepo) Fail(_ context.Context, id uuid.UUID, cause string, retryAt time.Time) error {
	s.failed = append(s.failed, stubFailure{id: id, cause: cause, retryAt: retryAt})
	return nil
}
func (s *stubErasureRepo) PendingOlderThan(context.Context, time.Duration) (int, error) {
	return s.pending, s.pendingErr
}

// stubBlobs is a Store that only answers DeletePrefix.
type stubBlobs struct {
	storage.Store
	prefixes []string
	err      error
	// failPrefix fails exactly one prefix, so a batch can hold a mailbox that
	// erases and one that does not.
	failPrefix string
}

func (s *stubBlobs) DeletePrefix(_ context.Context, prefix string) (int, error) {
	s.prefixes = append(s.prefixes, prefix)
	if s.err != nil {
		return 0, s.err
	}
	if s.failPrefix != "" && prefix == s.failPrefix {
		return 0, errors.New("bucket unreachable")
	}
	return 3, nil
}

// googleAnswering points the revoker at a stub for one test.
func googleAnswering(t *testing.T, status int) *string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("token")
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	prev := oauthrevoke.GoogleRevokeURL
	oauthrevoke.GoogleRevokeURL = srv.URL
	t.Cleanup(func() { oauthrevoke.GoogleRevokeURL = prev })
	return &got
}

// testEncrypter is the instance credential key, the one the refresh token was
// sealed under when the mailbox was connected.
func testEncrypter(t *testing.T) *encrypt.Encrypter {
	t.Helper()
	enc, err := encrypt.NewEncrypterFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encrypter: %v", err)
	}
	return enc
}

// gmailErasure is one queued erasure for a Gmail mailbox, with its refresh
// token sealed the way the delete copied it off the mailbox.
func gmailErasure(t *testing.T, enc *encrypt.Encrypter) repository.MailboxErasure {
	t.Helper()
	sealed, err := enc.Encrypt("1//0g-refresh")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return repository.MailboxErasure{
		EmailAccountID: uuid.New(),
		UserID:         uuid.New(),
		Email:          "box@test.local",
		Provider:       string(models.InboxProviderGoogle),
		RefreshToken:   sealed,
		BlobPrefix:     "users/u/emails/m/",
	}
}

// The whole job in one pass: the grant goes back to Google, the stored mail
// goes from the bucket, and the queue row goes with them.
func TestErasureRevokesTheGrantAndErasesTheMail(t *testing.T) {
	got := googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	e := gmailErasure(t, enc)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}
	blobs := &stubBlobs{}

	if err := NewMailboxErasureJob(repo, blobs, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(blobs.prefixes) != 1 || blobs.prefixes[0] != e.BlobPrefix {
		t.Errorf("erased prefixes %v, want %q", blobs.prefixes, e.BlobPrefix)
	}
	if len(repo.completed) != 1 || repo.completed[0] != e.EmailAccountID {
		t.Errorf("completed %v, want the mailbox's erasure finished", repo.completed)
	}
	if len(repo.failed) != 0 {
		t.Errorf("recorded failures on a clean pass: %v", repo.failed)
	}
	// The plaintext token, not the ciphertext sitting in the queue row. Sending
	// the sealed value gets a 400 that reads as "already revoked" for a grant
	// nobody ever touched.
	if *got != "1//0g-refresh" {
		t.Errorf("sent %q to the provider, want the opened refresh token", *got)
	}
	// Nothing is stamped on a clean pass: the half-done marks exist only so a
	// retry can skip work, and there is no retry. Writing them would be two
	// updates to a row being deleted in the same breath.
	if len(repo.revoked) != 0 || len(repo.erased) != 0 {
		t.Errorf("stamped half-done state on a row it then deleted (revoked=%v erased=%v)", repo.revoked, repo.erased)
	}
}

// The queue row holds the address of the mailbox the customer asked us to
// forget. Keeping it as a receipt would be keeping the thing being erased, so
// finishing means deleting it. The audit log is where the record lives.
func TestAFinishedErasureLeavesNoRowBehind(t *testing.T) {
	googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{gmailErasure(t, enc)}}

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 1 {
		t.Fatal("the erasure was not completed, so its row still names the mailbox")
	}
}

// Google being down must not make us forget that the bytes still need erasing,
// and must not repeat the half that already worked.
func TestAFailedRevocationKeepsTheErasureOutstanding(t *testing.T) {
	googleAnswering(t, http.StatusInternalServerError)
	enc := testEncrypter(t)
	e := gmailErasure(t, enc)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}
	blobs := &stubBlobs{}

	if err := NewMailboxErasureJob(repo, blobs, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(repo.completed) != 0 {
		t.Error("the erasure was completed with the grant still live at the provider")
	}
	if len(repo.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1 so it is retried", len(repo.failed))
	}
	if !strings.Contains(repo.failed[0].cause, "revoke") {
		t.Errorf("recorded cause %q, want it to name the revocation", repo.failed[0].cause)
	}
	// The half that worked is recorded, so the retry does not walk the prefix
	// again for a mailbox whose bytes are already gone.
	if len(repo.erased) != 1 {
		t.Errorf("the blob erasure that succeeded was not recorded (%v)", repo.erased)
	}
	if !repo.failed[0].retryAt.After(time.Now()) {
		t.Error("the retry was not scheduled into the future")
	}
}

// The mirror image: the provider answered, the bucket did not.
func TestAFailedBlobErasureKeepsTheErasureOutstanding(t *testing.T) {
	googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{gmailErasure(t, enc)}}
	blobs := &stubBlobs{err: errors.New("bucket unreachable")}

	if err := NewMailboxErasureJob(repo, blobs, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 0 {
		t.Error("the erasure was completed with the customer's mail still in the bucket")
	}
	if len(repo.revoked) != 1 {
		t.Errorf("the revocation that succeeded was not recorded (%v)", repo.revoked)
	}
	if len(repo.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(repo.failed))
	}
}

// One mailbox that cannot be erased must not hold up erasing everybody else's
// data. The batch is a queue, not a chain.
func TestOneStuckMailboxDoesNotBlockTheRest(t *testing.T) {
	googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	stuck := gmailErasure(t, enc)
	stuck.BlobPrefix = "users/u/emails/unreachable/"
	fine := gmailErasure(t, enc)

	repo := &stubErasureRepo{due: []repository.MailboxErasure{stuck, fine}}
	blobs := &stubBlobs{failPrefix: stuck.BlobPrefix}
	if err := NewMailboxErasureJob(repo, blobs, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 1 || repo.completed[0] != fine.EmailAccountID {
		t.Errorf("completed %v, want only the mailbox that could be erased", repo.completed)
	}
	if len(repo.failed) != 1 || repo.failed[0].id != stuck.EmailAccountID {
		t.Errorf("failed %v, want only the stuck mailbox", repo.failed)
	}
}

// An SMTP/IMAP mailbox has no grant. It must still have its stored mail
// erased, and must not be left outstanding waiting for a revocation that can
// never happen.
func TestAMailboxWithNoGrantStillHasItsMailErased(t *testing.T) {
	enc := testEncrypter(t)
	e := gmailErasure(t, enc)
	e.Provider = string(models.InboxProviderSMTPIMAP)
	e.RefreshToken = ""
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}
	blobs := &stubBlobs{}

	if err := NewMailboxErasureJob(repo, blobs, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(blobs.prefixes) != 1 {
		t.Errorf("erased %v, want the mailbox's stored mail", blobs.prefixes)
	}
	if len(repo.completed) != 1 {
		t.Errorf("completed %d erasures, want 1", len(repo.completed))
	}
}

// Outlook publishes no revocation endpoint. The mailbox must still finish
// erasing rather than retrying a call that does not exist forever.
func TestAnOutlookMailboxFinishesWithoutARevocationEndpoint(t *testing.T) {
	enc := testEncrypter(t)
	e := gmailErasure(t, enc)
	e.Provider = string(models.InboxProviderOutlook)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 1 {
		t.Errorf("completed %d erasures, want 1", len(repo.completed))
	}
	if len(repo.failed) != 0 {
		t.Errorf("retried a call Microsoft does not publish: %v", repo.failed)
	}
}

// An erasure nobody can finish is a customer who was told their data was
// deleted when it was not, so the job reports it rather than running green.
func TestOutstandingErasuresAreReported(t *testing.T) {
	repo := &stubErasureRepo{pending: 4}

	err := NewMailboxErasureJob(repo, &stubBlobs{}, nil).Run(context.Background())
	if err == nil {
		t.Fatal("a backlog of unfinished erasures was reported as a healthy run")
	}
	if !strings.Contains(err.Error(), "4") {
		t.Errorf("error = %q, want it to say how many are outstanding", err)
	}
}

// Backoff climbs and then stops climbing: a provider outage must not be
// hammered every minute, and an erasure must never stop being retried.
func TestRetryBackoffClimbsAndIsCapped(t *testing.T) {
	if got := retryDelay(0); got != erasureRetryBase {
		t.Errorf("first retry = %v, want %v", got, erasureRetryBase)
	}
	if retryDelay(3) <= retryDelay(1) {
		t.Error("backoff does not grow with attempts")
	}
	if got := retryDelay(50); got != erasureRetryMax {
		t.Errorf("retry after many attempts = %v, want it capped at %v", got, erasureRetryMax)
	}
}

// A token that cannot be opened, answered with the 400 that means "not a valid
// token", must NOT complete. That is the same answer the provider gives to
// ciphertext sent under the wrong key, which is what a restore with a
// mismatched CREDENTIALS_ENCRYPTION_KEY produces, so treating it as done would
// record a grant as revoked while it is still live on the customer's account.
func TestATokenThatCannotBeOpenedNeverCountsAsRevoked(t *testing.T) {
	got := googleAnswering(t, http.StatusBadRequest)
	e := gmailErasure(t, testEncrypter(t))
	e.RefreshToken = "not-openable"
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}

	// No encrypter wired at all, the shape of an instance without the key.
	if err := NewMailboxErasureJob(repo, &stubBlobs{}, nil).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 0 {
		t.Error("the erasure finished on a rejection that proves nothing about the grant")
	}
	if len(repo.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1 so it stays visible", len(repo.failed))
	}
	if !strings.Contains(repo.failed[0].cause, "CREDENTIALS_ENCRYPTION_KEY") {
		t.Errorf("cause = %q, want it to name the likely misconfiguration", repo.failed[0].cause)
	}
	if *got != "not-openable" {
		t.Errorf("sent %q, want the stored value to at least be tried", *got)
	}
}

// The same token accepted by the provider IS proof: a 200 means the grant is
// gone whether or not we understood what we sent, so it finishes.
func TestAnUnopenableTokenTheProviderAcceptsStillFinishes(t *testing.T) {
	googleAnswering(t, http.StatusOK)
	e := gmailErasure(t, testEncrypter(t))
	e.RefreshToken = "1//0g-legacy-plaintext"
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, nil).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.completed) != 1 {
		t.Errorf("completed %d erasures, want 1: the provider accepted the revocation", len(repo.completed))
	}
}

// The opposite of the above: a sealed token on an instance WITH the key must
// never be sent as ciphertext. The provider would answer 400 and the grant
// would be recorded as already gone while staying live on the customer's
// account, which is the exact failure this whole job exists to prevent.
func TestASealedTokenIsNeverSentAsCiphertext(t *testing.T) {
	got := googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	e := gmailErasure(t, enc)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{e}}

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, enc).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if *got == e.RefreshToken {
		t.Fatal("the sealed value was sent to the provider instead of the token")
	}
	if *got != "1//0g-refresh" {
		t.Errorf("sent %q, want the opened refresh token", *got)
	}
}

// A cancelled context is a shutdown, not a failed erasure. Recording failures
// on the way down would burn retry attempts and widen the backoff for work
// that was never attempted, so a redeploy would push every queued erasure
// further out every time.
func TestShutdownDoesNotCountAsAFailedErasure(t *testing.T) {
	googleAnswering(t, http.StatusOK)
	enc := testEncrypter(t)
	repo := &stubErasureRepo{due: []repository.MailboxErasure{gmailErasure(t, enc), gmailErasure(t, enc)}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, enc).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.failed) != 0 {
		t.Errorf("recorded %d failures while shutting down, want 0", len(repo.failed))
	}
	if len(repo.completed) != 0 {
		t.Errorf("completed %d erasures after the context was cancelled", len(repo.completed))
	}
}

// A backlog count that cannot be read is reported, not swallowed. Returning nil
// would show the job as green while nobody knows whether anything is stuck.
func TestAnUnreadableBacklogIsReported(t *testing.T) {
	repo := &stubErasureRepo{pendingErr: errors.New("database down")}

	if err := NewMailboxErasureJob(repo, &stubBlobs{}, nil).Run(context.Background()); err == nil {
		t.Fatal("an unreadable backlog was reported as a healthy run")
	}
}
