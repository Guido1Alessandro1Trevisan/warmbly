package repository

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A provider error body is arbitrary remote text. Cutting one mid-character
// yields invalid UTF-8, which Postgres rejects, which would lose the attempt
// count and the backoff along with the message: the erasure would then retry
// at the base delay forever instead of backing off.
func TestFailedErasureCauseStaysValidUTF8(t *testing.T) {
	for _, cause := range []string{
		strings.Repeat("é", 600),
		strings.Repeat("🔒", 400),
		strings.Repeat("a", 499) + "😀",
		"",
		strings.Repeat("ok", 10),
	} {
		got := truncateRunes(cause, 500)
		if !utf8.ValidString(got) {
			t.Errorf("truncating %d bytes produced invalid UTF-8", len(cause))
		}
		if n := utf8.RuneCountInString(got); n > 500 {
			t.Errorf("kept %d runes, want at most 500", n)
		}
	}
}

// Invalid bytes from a provider must not reach the column either.
func TestFailedErasureCauseDropsInvalidBytes(t *testing.T) {
	if got := truncateRunes("bad\xff\xfebytes", 500); !utf8.ValidString(got) || strings.Contains(got, "\xff") {
		t.Errorf("got %q, want the invalid bytes dropped", got)
	}
}
