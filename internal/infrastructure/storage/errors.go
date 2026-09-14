package storage

import (
	"errors"
	"strings"
)

var (
	// ErrNotFound is returned by Get when the key doesn't exist.
	ErrNotFound = errors.New("storage: key not found")

	// ErrUnsupported is returned when an implementation doesn't support an
	// optional operation (e.g. PresignedGetURL on the filesystem store).
	ErrUnsupported = errors.New("storage: operation not supported by this backend")
)

// MinPrefixSegments is how deep a prefix must be before DeletePrefix will act
// on it. The keys it is meant for look like
// "users/<user>/emails/<mailbox>/", so four is the shallowest real caller and
// anything above the per-mailbox directory is a mistake, not a request.
const MinPrefixSegments = 4

// ErrUnsafePrefix is returned by DeletePrefix for a prefix that does not end
// in "/", contains "..", or names fewer than MinPrefixSegments segments.
// Refusing is the whole safety property: DeletePrefix("") empties the bucket.
var ErrUnsafePrefix = errors.New("storage: refusing to delete an empty or overly broad prefix")

// CheckPrefix reports whether prefix is one DeletePrefix may act on. Every
// implementation calls it first, so the rule is stated once.
func CheckPrefix(prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") || strings.Contains(prefix, "..") {
		return ErrUnsafePrefix
	}
	n := 0
	for _, seg := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if seg == "" {
			return ErrUnsafePrefix
		}
		n++
	}
	if n < MinPrefixSegments {
		return ErrUnsafePrefix
	}
	return nil
}
