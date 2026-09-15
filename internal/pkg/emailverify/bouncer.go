package emailverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	bouncerAPI          = "https://api.usebouncer.com"
	bouncerTimeout      = 35 * time.Second
	bouncerProbeTimeout = 30
)

// Bouncer verifies addresses through Bouncer's real-time API.
type Bouncer struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewBouncer constructs a Bouncer client. baseURL is only for controlled local verification.
func NewBouncer(apiKey, baseURL string) *Bouncer {
	if baseURL == "" {
		baseURL = bouncerAPI
	}
	return &Bouncer{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: bouncerTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type bouncerResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Score  int    `json:"score"`
	Domain struct {
		AcceptAll  string `json:"acceptAll"`
		Disposable string `json:"disposable"`
	} `json:"domain"`
	Account struct {
		Role        string `json:"role"`
		Disabled    string `json:"disabled"`
		FullMailbox string `json:"fullMailbox"`
	} `json:"account"`
}

func (b *Bouncer) Check(ctx context.Context, email string) (Result, error) {
	res := Result{Email: strings.ToLower(strings.TrimSpace(email)), CheckedAt: time.Now().UTC(), Status: StatusUnknown, Provider: ProviderBouncer}
	q := url.Values{}
	q.Set("email", res.Email)
	q.Set("timeout", fmt.Sprintf("%d", bouncerProbeTimeout))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/v1.1/email/verify?"+q.Encode(), nil)
	if err != nil {
		res.Reason = "bouncer request could not be created"
		return res, errors.New("bouncer: request could not be created")
	}
	req.Header.Set("x-api-key", b.apiKey)
	resp, err := b.client.Do(req)
	if err != nil {
		// net/http errors can include the URL, whose query contains the recipient.
		res.Reason = "bouncer could not be reached"
		return res, errors.New("bouncer: request failed")
	}
	defer resp.Body.Close()
	if err := bouncerStatusError(resp.StatusCode); err != nil {
		res.Reason = "bouncer verification was unavailable"
		return res, err
	}
	var out bouncerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		res.Reason = "bouncer answered with an unreadable body"
		return res, errors.New("bouncer: unreadable response")
	}
	switch out.Status {
	case "deliverable":
		res.Status = StatusValid
	case "risky":
		res.Status = StatusRisky
	case "undeliverable":
		res.Status = StatusInvalid
	case "unknown":
		res.Status = StatusUnknown
	default:
		res.Reason = "bouncer returned an unrecognised status"
		return res, errors.New("bouncer: unrecognised status")
	}
	res.Confidence = out.Score
	res.HasMX = out.Reason != "invalid_domain" && out.Reason != "dns_error"
	switch {
	case out.Domain.Disposable == "yes":
		res.Status, res.SubStatus = StatusInvalid, SubStatusDisposable
	case out.Account.Disabled == "yes":
		res.Status = StatusInvalid
	case out.Account.FullMailbox == "yes":
		res.Status, res.SubStatus = StatusRisky, SubStatusMailboxFull
	case out.Account.Role == "yes":
		res.Status, res.SubStatus = StatusRisky, SubStatusRole
	case out.Domain.AcceptAll == "yes":
		res.IsCatchAll, res.SubStatus = true, SubStatusCatchAll
		if res.Status == StatusValid {
			res.Status = StatusRisky
		}
	}
	res.Reason = "bouncer: " + out.Reason
	return res, nil
}

func (b *Bouncer) Account(ctx context.Context) (*int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/v1.1/credits", nil)
	if err != nil {
		return nil, errors.New("bouncer: request could not be created")
	}
	req.Header.Set("x-api-key", b.apiKey)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, errors.New("bouncer: request failed")
	}
	defer resp.Body.Close()
	if err := bouncerStatusError(resp.StatusCode); err != nil {
		return nil, err
	}
	var out struct {
		Credits int `json:"credits"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, errors.New("bouncer: unreadable response")
	}
	return &out.Credits, nil
}

func (b *Bouncer) ObservesBalance() bool { return true }

func bouncerStatusError(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrProviderKey
	case http.StatusPaymentRequired:
		return ErrProviderCredits
	default:
		return fmt.Errorf("bouncer: HTTP %d", status)
	}
}
