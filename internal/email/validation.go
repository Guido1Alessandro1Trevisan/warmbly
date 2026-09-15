package email

import (
	"net/mail"
	"strings"
)

func IsValid(email string) bool {
	_, err := mail.ParseAddress(email)
	return err == nil
}

// Normalize returns an address in the form contacts are stored in: trimmed,
// lowercased, and stripped of any display name, because mail.ParseAddress
// accepts `Dana Reyes <dana@acme.com>` and storing that whole string as the
// address would send to nobody. ok is false for anything it refuses.
func Normalize(addr string) (string, bool) {
	a, err := mail.ParseAddress(strings.TrimSpace(addr))
	if err != nil {
		return "", false
	}
	return strings.ToLower(a.Address), true
}
