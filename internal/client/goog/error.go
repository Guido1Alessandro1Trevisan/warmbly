package goog

import (
	"errors"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/warmbly/warmbly/internal/errx"
	"github.com/warmbly/warmbly/internal/pkg/mailauth"
	"google.golang.org/api/googleapi"
)

// IsThreadRefusal reports a send Gmail rejected because of the threadId it was
// given rather than because of the message. Gmail only files a message in a
// thread when the subject and the reference headers line up with it, and the
// thread has to still exist in this mailbox; a moved, deleted or foreign
// thread is a 400/404 naming it. The send provably did not happen, so the
// caller can safely try again without the thread handle and deliver the email
// as its own conversation instead of failing the step.
func IsThreadRefusal(err error) bool {
	if err == nil {
		return false
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	if gerr.Code != 400 && gerr.Code != 404 {
		return false
	}
	return strings.Contains(strings.ToLower(gerr.Message), "thread")
}

func HandleError(err error) *errx.MailError {
	if err == nil {
		return nil
	}
	// errors.As, not a type assertion: the API client wraps its error on some
	// paths, and an unwrapped assertion sends a real 401 down the transport
	// branch below.
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case 401:
			return errx.ErrMailGoogleAuth
		case 402:
			return errx.ErrMailGooglePayment
		case 403:
			return errx.ErrMailGoogleForbidden(gerr.Message)
		default:
			respErr := errx.ErrMailGoogleUnknown(gerr.Code, gerr.Message)
			log.Debug().Err(err).Msg("Google Api Error")
			return respErr
		}
	}

	// The token source runs inside the API call, so a grant the user revoked
	// (or Google expired) never reaches Gmail to become a 401: it fails the
	// call itself and would otherwise read as an unreachable server, promising
	// a retry that can never succeed.
	if f := mailauth.ClassifyTokenError(err); f.Refused() {
		log.Debug().
			Str("oauth_error", f.ErrorCode).
			Str("oauth_description", f.Description).
			Int("status", f.Status).
			Bool("revoked", f.Revoked()).
			Msg("Gmail token refresh refused")
		if f.Revoked() {
			return errx.ErrMailGoogleAuth
		}
	}

	// Non-API failures (DNS, TLS, timeouts) are transient transport errors.
	// Returning nil here would silently swallow them and leave callers holding
	// a typed-nil *MailError in an error interface.
	log.Debug().Err(err).Msg("Google transport error")
	return errx.ErrMailServerUnreachable
}
