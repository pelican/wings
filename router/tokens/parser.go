package tokens

import (
	"errors"
	"time"

	"github.com/gbrlsnchs/jwt/v3"

	"github.com/pelican/wings/config"
)

// ErrInvalidToken is returned by ParseToken when a token is malformed, has an
// invalid signature or has expired. It wraps the underlying jwt error, so checks
// such as errors.Is(err, jwt.ErrExpValidation) keep working.
var ErrInvalidToken = errors.New("tokens: invalid or expired token")

type TokenData interface {
	GetPayload() *jwt.Payload
}

// Validates the provided JWT against the known secret for the Daemon and returns the
// parsed data. This function DOES NOT validate that the token is valid for the connected
// server, nor does it ensure that the user providing the token is able to actually do things.
//
// This simply returns a parsed token.
func ParseToken(token []byte, data TokenData) error {
	verifyOptions := jwt.ValidatePayload(
		data.GetPayload(),
		jwt.ExpirationTimeValidator(time.Now()),
	)

	if _, err := jwt.Verify(token, config.GetJwtAlgorithm(), &data, verifyOptions); err != nil {
		return invalidTokenError{err: err}
	}
	return nil
}

// invalidTokenError matches both ErrInvalidToken and the underlying jwt error,
// and keeps the message of the jwt error, which is sent to websocket clients.
type invalidTokenError struct {
	err error
}

func (e invalidTokenError) Error() string { return e.err.Error() }

func (e invalidTokenError) Unwrap() []error { return []error{ErrInvalidToken, e.err} }
