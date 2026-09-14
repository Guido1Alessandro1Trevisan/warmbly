package oauthrevoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/warmbly/warmbly/internal/models"
)

// serve points GoogleRevokeURL at a stub for one test and records what arrived.
func serve(t *testing.T, status int, body string) *string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("token")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	prev := GoogleRevokeURL
	GoogleRevokeURL = srv.URL
	t.Cleanup(func() { GoogleRevokeURL = prev })
	return &got
}

// The point of the whole package: the refresh token reaches Google, because
// deleting our copy of it leaves the grant on the customer's account.
func TestGoogleGrantIsHandedBack(t *testing.T) {
	got := serve(t, http.StatusOK, "")

	res, err := Revoke(context.Background(), models.InboxProviderGoogle, "1//0g-refresh")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if res != Revoked {
		t.Errorf("result = %q, want %q", res, Revoked)
	}
	if *got != "1//0g-refresh" {
		t.Errorf("sent token %q, want the refresh token", *got)
	}
}

// Google answers 400 for a token that is already revoked or expired. Treating
// that as a failure would leave the erasure outstanding forever over a grant
// that is already gone, which is the one outcome we were trying to reach.
func TestAnAlreadyRevokedGrantIsNotAFailure(t *testing.T) {
	serve(t, http.StatusBadRequest, `{"error":"invalid_token"}`)

	res, err := Revoke(context.Background(), models.InboxProviderGoogle, "stale")
	if err != nil {
		t.Fatalf("an already-revoked grant was reported as an error: %v", err)
	}
	if res != AlreadyGone {
		t.Errorf("result = %q, want %q", res, AlreadyGone)
	}
}

// Anything else is Google being unavailable, and must be retried rather than
// recorded as a revocation that never happened.
func TestAProviderOutageIsRetryable(t *testing.T) {
	serve(t, http.StatusInternalServerError, "upstream boom")

	if _, err := Revoke(context.Background(), models.InboxProviderGoogle, "live"); err == nil {
		t.Fatal("a 500 from Google was reported as a successful revocation")
	}
}

// Microsoft publishes no revocation endpoint for delegated tokens, and the one
// Graph call that ends sessions signs the person out of every application.
// Saying so is the honest answer; inventing a call is not.
func TestOutlookReportsThatThereIsNoEndpoint(t *testing.T) {
	res, err := Revoke(context.Background(), models.InboxProviderOutlook, "M.C123_refresh")
	if err != nil {
		t.Fatalf("outlook: %v", err)
	}
	if res != NoEndpoint {
		t.Errorf("result = %q, want %q", res, NoEndpoint)
	}
}

// An SMTP/IMAP mailbox has a password, not a grant, and its row is already
// gone. Nothing to call, and no error either.
func TestSMTPIMAPHasNoGrant(t *testing.T) {
	res, err := Revoke(context.Background(), models.InboxProviderSMTPIMAP, "")
	if err != nil {
		t.Fatalf("smtp_imap: %v", err)
	}
	if res != NoToken {
		t.Errorf("result = %q, want %q", res, NoToken)
	}
}

// A Gmail mailbox whose token row was already gone must not produce a request
// with an empty token, which Google answers 400 and which reads in the log as
// "already revoked" for a grant nobody ever checked.
func TestAnEmptyTokenIsNeverSent(t *testing.T) {
	got := serve(t, http.StatusOK, "")

	res, err := Revoke(context.Background(), models.InboxProviderGoogle, "   ")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if res != NoToken {
		t.Errorf("result = %q, want %q", res, NoToken)
	}
	if *got != "" {
		t.Errorf("called the provider with %q for a mailbox that had no token", *got)
	}
}
