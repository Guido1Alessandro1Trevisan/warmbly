package dsn

import (
	"strings"
	"testing"
)

const permanentDSN = `This is the mail system at host mail.example.com.

I'm sorry to have to inform you that your message could not
be delivered to one or more recipients.

Final-Recipient: rfc822; nobody@invalid.example.com
Action: failed
Status: 5.1.1
Diagnostic-Code: smtp; 550 5.1.1 user unknown

--- Original message headers ---
From: sender@yourdomain.com
Message-ID: <camp-abc-123@yourdomain.com>
Subject: Quick question
`

const transientDSN = `Delivery is delayed.

Final-Recipient: rfc822; busy@example.com
Action: delayed
Status: 4.4.1
Message-ID: <camp-xyz-999@yourdomain.com>
`

func TestParsePermanent(t *testing.T) {
	r := Parse(permanentDSN)
	if !r.IsBounce || !r.Permanent {
		t.Fatalf("expected permanent bounce, got %+v", r)
	}
	if r.FailedRecipient != "nobody@invalid.example.com" {
		t.Errorf("failed recipient = %q", r.FailedRecipient)
	}
	if r.OriginalMessageID != "camp-abc-123@yourdomain.com" {
		t.Errorf("original message id = %q", r.OriginalMessageID)
	}
}

func TestParseTransientNotPermanent(t *testing.T) {
	r := Parse(transientDSN)
	if !r.IsBounce {
		t.Fatal("expected a bounce")
	}
	if r.Permanent {
		t.Error("4.x.x/delayed must not be permanent (would over-suppress)")
	}
}

func TestParseOrdinaryBody(t *testing.T) {
	r := Parse("Hi, thanks for reaching out, let's chat next week.")
	if r.IsBounce || r.Permanent {
		t.Errorf("ordinary mail misread as bounce: %+v", r)
	}
}

func TestDetect(t *testing.T) {
	if !Detect("Mail Delivery Subsystem <MAILER-DAEMON@google.com>", "", "") {
		t.Error("mailer-daemon not detected")
	}
	if !Detect("x@y.com", "Undeliverable: Quick question", "") {
		t.Error("bounce subject not detected")
	}
	if !Detect("x@y.com", "", "multipart/report; report-type=delivery-status") {
		t.Error("multipart/report not detected")
	}
	if Detect("alice@example.com", "Re: your email", "text/plain") {
		t.Error("ordinary reply misdetected as bounce")
	}
}

// Exchange Online NDR as Microsoft Graph returns it: one rendered HTML body,
// no message/delivery-status part, the remote reply quoted in prose.
const exchangeGraphNDR = `<html><body><p>Delivery has failed to these recipients or groups:</p>
<p><a href="mailto:lead@example.com">lead@example.com</a> (lead@example.com)<br>
Your message wasn't delivered. Please try resending the message.</p>
<p><b>Diagnostic information for administrators:</b></p>
<p>Generating server: MW4PR10MB6558.namprd10.prod.outlook.com<br>
lead@example.com<br>
Remote server returned '550 5.7.708 Service unavailable. Access denied, traffic not accepted from this IP. AS(7910)'</p>
<p>Original message headers:</p>
<pre>Received: from BN0PR10MB5271.namprd10.prod.outlook.com
From: Guido Trevisan &lt;guido@example.org&gt;
To: "lead@example.com" &lt;lead@example.com&gt;
Subject: overpaying?
Message-ID: &lt;BN0PR10MB5271ABCDEF@BN0PR10MB5271.namprd10.prod.outlook.com&gt;
</pre></body></html>`

func TestParseExchangeGraphNDR(t *testing.T) {
	r := Parse(exchangeGraphNDR)
	if !r.IsBounce || !r.Permanent {
		t.Fatalf("expected permanent bounce, got %+v", r)
	}
	if r.FailedRecipient != "lead@example.com" {
		t.Errorf("failed recipient = %q", r.FailedRecipient)
	}
	if r.OriginalMessageID != "BN0PR10MB5271ABCDEF@BN0PR10MB5271.namprd10.prod.outlook.com" {
		t.Errorf("original message id = %q", r.OriginalMessageID)
	}
	if !strings.HasPrefix(r.Diagnostic, "550 5.7.708 Service unavailable") {
		t.Errorf("diagnostic = %q", r.Diagnostic)
	}
	if !Detect("Microsoft Outlook <MicrosoftExchange329e71ec88ae4615bbc36ab6ce41109e@contoso.onmicrosoft.com>", "Non recapitabile: overpaying?", "") {
		t.Error("Exchange NDR sender not detected")
	}
}

func TestParseExchangeDelayedNotPermanent(t *testing.T) {
	r := Parse(`Delivery has failed to these recipients or groups: lead@example.com
Remote server returned '451 4.4.1 Connection timed out'`)
	if !r.IsBounce || r.Permanent {
		t.Errorf("4.x.x remote reply must stay transient: %+v", r)
	}
}
