package config

import (
	"os"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
)

type Oauth2Inbox struct {
	Google  *oauth2.Config
	Outlook *oauth2.Config
}

func GoogleOauth2Inbox(baseURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     os.Getenv("BOX_GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("BOX_GOOGLE_CLIENT_SECRET"),
		RedirectURL:  baseURL + "/addresses/google/callback",
		Scopes: []string{
			gmail.GmailComposeScope,
			gmail.GmailMetadataScope,
			gmail.GmailModifyScope,
			gmail.GmailSendScope,
			gmail.GmailSettingsBasicScope,
			gmail.GmailReadonlyScope,
		},
		Endpoint: google.Endpoint,
	}
}

// OutlookOauth2Inbox configures delegated Microsoft Graph access for Outlook /
// Microsoft 365 mailboxes. Graph is the transport now (RAW MIME sendMail + delta
// sync), so we request Graph scopes rather than the legacy IMAP/SMTP scopes:
// Mail.Send (send), Mail.ReadWrite (delta sync + warmup move/mark/flag),
// User.Read (resolve the mailbox owner via /me), and offline_access (refresh
// token). None require tenant admin consent by default. BOX_OUTLOOK_TENANT_ID
// pins both authorization and refresh to one Entra organization when set.
func OutlookOauth2Inbox(baseURL string) *oauth2.Config {
	tenant := strings.TrimSpace(os.Getenv("BOX_OUTLOOK_TENANT_ID"))
	if tenant == "" {
		tenant = "common"
	} else if parsed, err := uuid.Parse(tenant); err == nil {
		tenant = parsed.String()
	} else {
		// Never turn malformed single-tenant configuration into /common access.
		tenant = "invalid-tenant-id"
	}
	endpoint := "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/"
	return &oauth2.Config{
		ClientID:     os.Getenv("BOX_OUTLOOK_CLIENT_ID"),
		ClientSecret: os.Getenv("BOX_OUTLOOK_CLIENT_SECRET"),
		RedirectURL:  baseURL + "/addresses/outlook/callback",
		Scopes: []string{
			"openid",
			"email",
			"profile",
			"offline_access",
			"https://graph.microsoft.com/User.Read",
			"https://graph.microsoft.com/Mail.Send",
			"https://graph.microsoft.com/Mail.ReadWrite",
		},
		Endpoint: oauth2.Endpoint{
			AuthURL:  endpoint + "authorize",
			TokenURL: endpoint + "token",
		},
	}
}
