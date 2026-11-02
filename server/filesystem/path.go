package filesystem

import (
	"emperror.dev/errors"
)

// Checks if the given file or path is in the server's file denylist. If so, an Error
// is returned, otherwise nil is returned.
//
// The path is checked both as provided and after being cleaned, so that a request
// such as "foo/../config.yml" cannot be used to get around an anchored pattern like
// "/config.yml".
//
// TODO: symlinks are not resolved, so a symlink pointing at a denylisted file is not
// itself treated as denylisted (this was already the case before the move to os.Root).
func (fs *Filesystem) IsIgnored(paths ...string) error {
	for _, p := range paths {
		if fs.denylist.MatchesPath(p) || fs.denylist.MatchesPath(normalize(p)) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: normalize(p)})
		}
	}
	return nil
}
