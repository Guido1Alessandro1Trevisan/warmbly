// Package dsn parses delivery-status notifications (bounce messages, RFC 3464)
// well enough to decide whether a permanent failure occurred, who it was for,
// and which original message it concerns. It is deliberately conservative:
// callers act (suppress a recipient) only on a confirmed permanent failure, so
// a soft/transient bounce is never treated as hard.
//
// Two shapes are understood: the machine-readable message/delivery-status
// part (IMAP and Gmail expose it), and the rendered Exchange Online NDR that
// Microsoft Graph returns as one HTML body ("Delivery has failed to these
// recipients or groups … Remote server returned '550 5.7.708 …'"), which has
// no delivery-status part at all.
package dsn

import (
	"regexp"
	"strings"
)

// Report is the extracted result of parsing a bounce message.
type Report struct {
	// IsBounce is true when the message looks like a delivery-status report at
	// all (worth acting on); false for ordinary mail.
	IsBounce bool
	// Permanent is true only when a 5.x.x status or an explicit permanent
	// "Action: failed" was found. Transient (4.x.x / delayed) stays false.
	Permanent bool
	// FailedRecipient is the address that bounced (Final/Original-Recipient).
	FailedRecipient string
	// OriginalMessageID is the Message-ID of the original outbound message the
	// bounce concerns, recovered from the returned headers section.
	OriginalMessageID string
	// Diagnostic is the remote server's reply text ("550 5.7.708 Service
	// unavailable …") when the report carries one. It is the reason callers
	// should record: it says whether the recipient or the sender was rejected.
	Diagnostic string
}

var (
	reStatus     = regexp.MustCompile(`(?im)^\s*Status:\s*([245])\.\d{1,3}\.\d{1,3}`)
	reAction     = regexp.MustCompile(`(?im)^\s*Action:\s*(failed|delayed|delivered|relayed|expanded)`)
	reFinalRcpt  = regexp.MustCompile(`(?im)^\s*(?:Final|Original)-Recipient:\s*[^;\r\n]*;\s*<?([^\s<>]+@[^\s<>]+?)>?\s*$`)
	reDiagnostic = regexp.MustCompile(`(?im)^\s*Diagnostic-Code:\s*[^;\r\n]*;\s*(([245])\d\d[^\r\n]*)`)
	// Message-ID may sit in a returned-headers text block (line-anchored) or in
	// a rendered HTML body where the brackets are entity-escaped and lines are
	// tags, so it is matched wherever it appears; the last one wins.
	reMessageID = regexp.MustCompile(`(?i)(?:^|[\s>;])Message-ID:\s*(?:<|&lt;)([^<>\s&]+)(?:>|&gt;)`)

	// Exchange Online NDR as rendered for the mailbox owner (the only form
	// Microsoft Graph returns). The diagnostic block quotes the remote reply.
	reExchangeFailed = regexp.MustCompile(`(?i)delivery has failed to these recipients or groups`)
	reRemoteReply    = regexp.MustCompile(`(?i)remote server returned\s*(?:'|&#39;|&apos;|"|&quot;)?\s*(([245])\d\d(?:[ -]+[245]\.\d{1,3}\.\d{1,3})?[^'"<\r\n]*)`)
	reEmail          = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reTag            = regexp.MustCompile(`<[^>]*>`)
)

// senderMarkers identify a machine bounce source in the From line.
// "microsoftexchange" is the Exchange Online NDR sender
// (MicrosoftExchange329e71ec88ae4615bbc36ab6ce41109e@<tenant>).
var senderMarkers = []string{"mailer-daemon", "postmaster@", "mail-daemon", "microsoftexchange"}

// subjectMarkers are common bounce subjects across providers.
var subjectMarkers = []string{
	"undeliverable", "undelivered mail", "delivery status notification",
	"returned mail", "delivery failure", "mail delivery failed",
	"failure notice", "message not delivered", "delivery incomplete",
}

// Detect reports whether an inbound message looks like a bounce, using only the
// cheap envelope signals (no body parse). Callers gate the full Parse on this.
func Detect(from, subject, contentType string) bool {
	f := strings.ToLower(from)
	for _, m := range senderMarkers {
		if strings.Contains(f, m) {
			return true
		}
	}
	if strings.Contains(strings.ToLower(contentType), "multipart/report") {
		return true
	}
	s := strings.ToLower(subject)
	for _, m := range subjectMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Parse extracts bounce details from the message body (the DSN parts, or the
// rendered Exchange notice). It is safe on non-DSN bodies: fields simply stay
// empty. Permanence is decided from the machine-readable Status /
// Diagnostic-Code / Action fields or the quoted remote reply code, never from
// the human-readable prose, so a "delayed" notice is never a permanent bounce.
func Parse(body string) Report {
	var r Report

	if m := reStatus.FindStringSubmatch(body); m != nil {
		r.IsBounce = true
		r.Permanent = m[1] == "5"
	}
	if m := reDiagnostic.FindStringSubmatch(body); m != nil {
		r.IsBounce = true
		r.Diagnostic = strings.TrimSpace(m[1])
		if !r.Permanent {
			r.Permanent = m[2] == "5"
		}
	}
	if a := reAction.FindStringSubmatch(body); a != nil {
		r.IsBounce = true
		if strings.EqualFold(a[1], "failed") && r.hasNo4xx(body) {
			// "failed" is permanent per RFC 3464, but a co-present 4.x.x status
			// (retry-then-fail) means transient — defer to the status code.
			r.Permanent = true
		}
	}

	if m := reFinalRcpt.FindStringSubmatch(body); m != nil {
		r.FailedRecipient = strings.TrimSpace(m[1])
	}

	r.parseExchange(body)

	// The last Message-ID in the body belongs to the returned original message
	// (the DSN's own id, if present, is a header, not in the body parts).
	if ids := reMessageID.FindAllStringSubmatch(body, -1); len(ids) > 0 {
		r.OriginalMessageID = strings.TrimSpace(ids[len(ids)-1][1])
	}

	return r
}

// parseExchange reads the rendered Exchange Online notice. The remote reply
// code decides permanence exactly as Diagnostic-Code does for a DSN part; the
// failed address is the first one named after the failure banner.
func (r *Report) parseExchange(body string) {
	loc := reExchangeFailed.FindStringIndex(body)
	if loc == nil {
		return
	}
	r.IsBounce = true
	if m := reRemoteReply.FindStringSubmatch(body); m != nil {
		if r.Diagnostic == "" {
			r.Diagnostic = trimQuoteEntities(strings.TrimSpace(m[1]))
		}
		if !r.Permanent {
			r.Permanent = m[2] == "5"
		}
	}
	if r.FailedRecipient == "" {
		after := body[loc[1]:]
		if len(after) > 2000 {
			after = after[:2000]
		}
		// Strip tags first so a mailto: link or a wrapped address does not
		// split the match.
		if addr := reEmail.FindString(reTag.ReplaceAllString(after, " ")); addr != "" {
			r.FailedRecipient = addr
		}
	}
}

// trimQuoteEntities drops the closing quote an HTML body leaves on the remote
// reply as an entity (&#39; &apos; &quot;), which the regexp cannot exclude.
func trimQuoteEntities(s string) string {
	for _, ent := range []string{"&#39;", "&apos;", "&quot;"} {
		s = strings.TrimSuffix(s, ent)
	}
	return strings.TrimSpace(s)
}

func (r Report) hasNo4xx(body string) bool {
	if m := reStatus.FindStringSubmatch(body); m != nil {
		return m[1] != "4"
	}
	return true
}
