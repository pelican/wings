package sftp

import (
	"io"
	"os"

	pkgsftp "github.com/pkg/sftp"
)

// ErrSSHQuotaExceeded extends the default SFTP server to return a quota exceeded error to the client.
//
// pkgsftp.ErrSSHFxOk is a typed constant of pkg/sftp's unexported fxerr type, so constant
// arithmetic on it (ErrSSHFxOk + 15, where ErrSSHFxOk == 0) yields a genuine fxerr with the
// SSH_FX_QUOTA_EXCEEDED code. pkg/sftp's statusFromError then matches it natively through
// errors.As (via Unwrap), without any reflection. If the dependency changes in an
// incompatible way, this breaks at compile time rather than silently falling back to a
// generic SSH_FX_FAILURE.
//
// @see https://tools.ietf.org/id/draft-ietf-secsh-filexfer-13.txt
var ErrSSHQuotaExceeded error = quotaExceededError{}

type quotaExceededError struct{}

func (quotaExceededError) Error() string { return "Quota Exceeded" }

// Unwrap exposes the real pkg/sftp fxerr so errors.As can find it.
func (quotaExceededError) Unwrap() error { return pkgsftp.ErrSSHFxOk + 15 }

type ListerAt []os.FileInfo

// ListAt returns the number of entries copied and an io.EOF error if we made it to the end of the file list.
// Take a look at the pkg/sftp godoc for more information about how this function should work.
func (l ListerAt) ListAt(f []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}

	if n := copy(f, l[offset:]); n < len(f) {
		return n, io.EOF
	} else {
		return n, nil
	}
}
