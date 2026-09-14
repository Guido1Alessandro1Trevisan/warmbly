// Package oauthrevoke hands a mailbox's OAuth grant back to the provider that
// issued it.
//
// Deleting our copy of a refresh token is not revocation. The grant stays on
// the user's account, the app keeps appearing in their connected-apps list,
// and anything else holding a copy of the token keeps working. Google's API
// Services User Data Policy requires that a user be able to have their data
// deleted, and leaving a live grant behind after they disconnected a mailbox
// is the clearest way to fail that.
package oauthrevoke

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/warmbly/warmbly/internal/models"
)

// GoogleRevokeURL is the endpoint from Google's OAuth 2.0 documentation.
// Revoking a refresh token revokes every access token derived from it and
// removes the app from the user's account. A var so tests can point it at a
// server that answers; nothing else reassigns it.
var GoogleRevokeURL = "https://oauth2.googleapis.com/revoke"

// Result is what became of the grant. Every value except ErrorResult means
// there is nothing further this instance can do for that mailbox.
type Result string

const (
	// Revoked: the provider accepted the revocation.
	Revoked Result = "revoked"
	// AlreadyGone: the provider says the token is not valid, which is the
	// answer for a grant the user already removed, or one that expired. The
	// outcome we wanted is the outcome we have.
	AlreadyGone Result = "already_gone"
	// NoEndpoint: the provider publishes no revocation endpoint. Microsoft is
	// the case that matters. The identity platform has no RFC 7009 endpoint
	// for delegated tokens, and the only Graph call that ends sessions
	// (revokeSignInSessions) signs the person out of every application rather
	// than removing ours, which is not a thing to do to someone who
	// disconnected one mailbox. Our copy of the token is destroyed either way;
	// removing the grant is done by the user at their Microsoft account.
	NoEndpoint Result = "no_endpoint"
	// NoToken: nothing was stored to revoke. SMTP/IMAP mailboxes and any OAuth
	// mailbox whose token row was already gone.
	NoToken Result = "no_token"
)

// MicrosoftConsentURL is where a person removes this app from their Microsoft
// account. Quoted to users rather than called by us.
const MicrosoftConsentURL = "https://myaccount.microsoft.com/privacy"

// client is deliberately short-deadlined: a revoke that hangs is retried by
// the caller on its next pass, and the erasure job must not be held by one
// unresponsive provider.
var client = &http.Client{Timeout: 15 * time.Second}

// maxBody caps what an error message quotes back.
const maxBody = 4 << 10

// Revoke hands the grant back for one mailbox. A nil error means the grant is
// gone, or that no endpoint exists to send it to; the caller records the
// Result and stops. An error means the attempt should be repeated.
func Revoke(ctx context.Context, provider models.InboxProvider, refreshToken string) (Result, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return NoToken, nil
	}

	switch provider {
	case models.InboxProviderGoogle:
		return revokeGoogle(ctx, refreshToken)
	case models.InboxProviderOutlook:
		return NoEndpoint, nil
	default:
		// SMTP/IMAP has a password, not a grant, and the row holding it is
		// already gone.
		return NoToken, nil
	}
}

func revokeGoogle(ctx context.Context, refreshToken string) (Result, error) {
	form := url.Values{"token": {refreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GoogleRevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("google revoke: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	switch {
	case resp.StatusCode == http.StatusOK:
		return Revoked, nil
	case resp.StatusCode == http.StatusBadRequest:
		// Google answers 400 invalid_token for a token that is already
		// revoked, already expired, or was never valid. Retrying cannot
		// improve on that, and treating it as a failure would keep an
		// erasure outstanding forever over a grant that is already gone.
		return AlreadyGone, nil
	default:
		return "", fmt.Errorf("google revoke: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// ErrNoProvider is returned for an empty provider, which means a caller built
// an erasure row without one.
var ErrNoProvider = errors.New("oauthrevoke: no provider")
