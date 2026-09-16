package emailverify

import (
	"net"
	"strconv"
	"strings"
)

// A 5xx reply to RCPT TO does NOT on its own mean the recipient is unknown.
// Servers use the same code range to reject the PROBE — a non-FQDN HELO, an
// unacceptable envelope sender, a blocklisted IP, relay policy. Postfix in
// particular defers its HELO/sender restrictions to RCPT time
// (smtpd_delay_reject is on by default), so a rejected HELO surfaces as
// "504 5.5.2 <localhost>: Helo command rejected: need fully-qualified
// hostname" *on the RCPT command*. Reading that as "this address is invalid"
// permanently drops a perfectly good contact, which is exactly what issue #200
// hit. Only a reply that talks about the RECIPIENT may produce StatusInvalid;
// everything else degrades to StatusUnknown, which still sends.

// recipientMarkers are phrases that name the RECIPIENT as the problem. They are
// decisive regardless of the numeric code, because providers disagree about
// which code carries them (Microsoft answers a missing mailbox with 5.4.1,
// some Exim builds with 554).
var recipientMarkers = []string{
	"user unknown",
	"unknown user",
	"no such user",
	"no such recipient",
	"no such mailbox",
	"user not found",
	"recipient not found",
	"recipient unknown",
	"unknown recipient",
	"recipient address rejected",
	"recipient rejected",
	"invalid recipient",
	"invalid mailbox",
	"mailbox unavailable",
	"mailbox not found",
	"mailbox does not exist",
	"mailbox is disabled",
	"mailbox disabled",
	"account has been disabled",
	"account is disabled",
	"account does not exist",
	"address does not exist",
	"does not exist",
	"doesn't exist",
	"unrouteable address",
	"unroutable address",
	"no mailbox here",
	"not a valid mailbox",
	"user doesn't have an account",
}

// probeMarkers are phrases that name OUR probe as the problem: the greeting,
// the envelope sender, the connecting IP, or a policy applied to the session.
// A reply carrying one of these says nothing about whether the mailbox exists.
var probeMarkers = []string{
	"helo",
	"ehlo",
	"fully-qualified",
	"fully qualified",
	"relay access denied",
	"relay denied",
	"relaying denied",
	"not permitted to relay",
	"unable to relay",
	"client host rejected",
	"client host blocked",
	"sender address rejected",
	"sender rejected",
	"sender verify failed",
	"sender not allowed",
	"authentication required",
	"auth required",
	"not authenticated",
	"not authorized",
	"blacklist",
	"blocklist",
	"blocked using",
	"listed in",
	"spamhaus",
	"reputation",
	"rate limit",
	"too many",
	"greylist",
	"grey-list",
	"try again",
	"temporarily",
	"service unavailable",
	"traffic not accepted",
	"not accepted from this ip",
	"connection refused",
	"policy reasons",
	"local policy",
	"security policy",
}

// enhancedStatus is an RFC 3463 status code (class.subject.detail) parsed off
// the front of a reply text, e.g. "5.1.1" in "5.1.1 <a@b>: User unknown".
type enhancedStatus struct {
	class, subject, detail int
	ok                     bool
}

func parseEnhancedStatus(msg string) enhancedStatus {
	field := strings.TrimSpace(msg)
	if i := strings.IndexAny(field, " \t"); i >= 0 {
		field = field[:i]
	}
	parts := strings.Split(strings.TrimSuffix(field, ":"), ".")
	if len(parts) != 3 {
		return enhancedStatus{}
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return enhancedStatus{}
		}
		nums[i] = n
	}
	if nums[0] != 2 && nums[0] != 4 && nums[0] != 5 {
		return enhancedStatus{}
	}
	return enhancedStatus{class: nums[0], subject: nums[1], detail: nums[2], ok: true}
}

// classifyPermanentRcpt decides whether a permanent (5xx) RCPT rejection is
// about the recipient (a hard invalid we may record) or about the probe (which
// must degrade to unknown so a good contact is never dropped).
func classifyPermanentRcpt(code int, msg string) (probeOutcome, string) {
	lower := strings.ToLower(msg)
	prefix := "recipient rejected (" + strconv.Itoa(code) + "): "

	// 1. An explicit recipient phrase is decisive. Checked first because
	//    Microsoft's "Recipient address rejected: Access denied" carries both a
	//    recipient phrase and a policy-sounding one.
	if containsAny(lower, recipientMarkers) {
		return probeRejected, prefix + msg
	}
	// 2. An explicit probe/session phrase means the server never judged the
	//    address at all.
	if containsAny(lower, probeMarkers) {
		return probeUnknown, "probe rejected, address not judged (" + strconv.Itoa(code) + "): " + msg
	}
	// 3. Otherwise trust the enhanced status code when the server sent one.
	if es := parseEnhancedStatus(msg); es.ok && es.class == 5 {
		switch es.subject {
		case 1: // Addressing. 5.1.7/5.1.8 describe the SENDER address.
			switch es.detail {
			case 1, 2, 3, 6, 10:
				return probeRejected, prefix + msg
			}
		case 2: // Mailbox. Only "disabled" means gone; full/too-large mailboxes exist.
			if es.detail == 1 {
				return probeRejected, prefix + msg
			}
		}
		// 5.3 system, 5.4 routing, 5.5 protocol, 5.6 content, 5.7 policy, and
		// every unlisted addressing/mailbox detail describe the session.
		return probeUnknown, "probe rejected, address not judged (" + strconv.Itoa(code) + "): " + msg
	}
	// 4. No enhanced status: only the codes that conventionally answer an
	//    unknown mailbox at RCPT count. 500-504 reject the COMMAND, 552 is a
	//    full mailbox, 554 is a catch-all "transaction failed".
	switch code {
	case 550, 551, 553:
		return probeRejected, prefix + msg
	}
	return probeUnknown, "probe rejected, address not judged (" + strconv.Itoa(code) + "): " + msg
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// reservedHeloTLDs never resolve on the public internet, so announcing one is
// as unusable as announcing a bare hostname.
var reservedHeloTLDs = map[string]bool{
	"local": true, "localdomain": true, "localhost": true, "internal": true,
	"invalid": true, "test": true, "example": true, "home": true, "lan": true,
	"corp": true, "intranet": true, "private": true,
}

// isFQDN reports whether host is a public, fully-qualified hostname usable as
// an SMTP HELO identity. A bare name ("localhost"), an IP literal, or a
// reserved-TLD name is refused by RFC-compliant servers
// (Postfix's reject_non_fqdn_helo_hostname), which is what turns a probe into
// a false "invalid address" verdict.
func isFQDN(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || len(h) > 253 || strings.ContainsAny(h, " \t\r\n:/@[]_") {
		return false
	}
	if net.ParseIP(h) != nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || reservedHeloTLDs[tld] {
		return false
	}
	for i := 0; i < len(tld); i++ {
		if tld[i] < 'a' || tld[i] > 'z' {
			return false
		}
	}
	return true
}
