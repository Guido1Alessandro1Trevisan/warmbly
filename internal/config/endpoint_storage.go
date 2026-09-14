package config

import "github.com/google/uuid"

var (
	// StorageEndpointMailboxPrefix is where everything one mailbox writes to
	// object storage lives. Erasing a mailbox deletes this prefix, so the two
	// are defined together: a body written outside it would survive the
	// mailbox being deleted, with nothing to notice.
	StorageEndpointMailboxPrefix = func(userID, emailID uuid.UUID) string {
		return "users/" + userID.String() + "/emails/" + emailID.String() + "/"
	}

	StorageEndpointEmailBody = func(userID, emailID, emailMessageID uuid.UUID) string {
		return StorageEndpointMailboxPrefix(userID, emailID) + emailMessageID.String() + ".emsg"
	}
)
