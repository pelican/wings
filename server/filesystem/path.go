package filesystem

import (
	fs2 "io/fs"
	"os"
	"path"
	"strings"

	"emperror.dev/errors"
)

// maxSymlinkHops is how many symlinks resolveInRoot follows before giving up,
// the same limit Linux uses (MAXSYMLINKS).
const maxSymlinkHops = 40

// Checks if the given file or path is in the server's file denylist. If so, an Error
// is returned, otherwise nil is returned.
//
// The path is checked as provided, after being cleaned, and after being cleaned as
// if it started at the server root. That way neither "foo/../config.yml" nor
// "../config.yml" can be used to get around an anchored pattern like "/config.yml";
// callers do not all clean a leading "/" off the same way before using the path.
//
// The path is also checked after resolving any symlinks in it, since os.Root
// follows symlinks that stay inside the server directory. Without that a link
// such as "notes -> config.yml" would give access to a denylisted file.
func (fs *Filesystem) IsIgnored(paths ...string) error {
	for _, p := range paths {
		if fs.matchesDenylist(p) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: normalize(p)})
		}
		if err := fs.isIgnoredThroughLink(p); err != nil {
			return err
		}
	}
	return nil
}

// IsIgnoredNoFollow is IsIgnored for operations that act on a directory entry
// itself rather than on what it points to: deleting and renaming. Symlinks in the
// parent directories are followed, since os.Root follows them too, but a symlink
// at the end of the path is not, so a link to a denylisted file can still be
// deleted or renamed without the file it points to being touched.
func (fs *Filesystem) IsIgnoredNoFollow(paths ...string) error {
	for _, p := range paths {
		if fs.matchesDenylist(p) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: normalize(p)})
		}
		if !fs.hasDenylist {
			continue
		}
		clean := normalize("/" + p)
		if clean == "." {
			continue
		}
		resolved := normalize("/" + path.Join(fs.resolveInRoot(path.Dir(clean)), path.Base(clean)))
		if resolved != clean && fs.matchesDenylist(resolved) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: resolved})
		}
	}
	return nil
}

// IsIgnoredTree is IsIgnoredNoFollow for renaming or moving from to to, where
// from may be a directory. Moving a directory moves everything inside it, so
// every entry under from is checked both where it is now and where it would
// end up under to. Otherwise renaming "plugins" would move a denylisted
// "plugins/secret.yml" out of its protected path, and renaming a directory
// onto "plugins" would put a file at that path.
//
// Like IsIgnoredNoFollow, a symlink is moved rather than followed, so a link to
// a directory is checked as a single entry.
func (fs *Filesystem) IsIgnoredTree(from, to string) error {
	if err := fs.IsIgnoredNoFollow(from, to); err != nil {
		return err
	}
	if !fs.hasDenylist {
		return nil
	}
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	from = normalize("/" + from)
	to = normalize("/" + to)
	if from == "." {
		return nil
	}
	st, err := root.Lstat(from)
	if err != nil || !st.IsDir() {
		// A missing source fails in the rename itself, and anything other than
		// a directory was checked above.
		return nil
	}
	return fs2.WalkDir(rootDirFS{root}, from, func(p string, _ fs2.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs2.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == from {
			return nil
		}
		rel := strings.TrimPrefix(p, from+"/")
		return fs.IsIgnoredNoFollow(p, path.Join(to, rel))
	})
}

// isIgnoredThroughLink returns an error if p, once its symlinks are resolved,
// is a different path that is in the denylist. It does not check p itself.
func (fs *Filesystem) isIgnoredThroughLink(p string) error {
	if !fs.hasDenylist {
		return nil
	}
	resolved := fs.resolveInRoot(p)
	if resolved != normalize("/"+p) && fs.matchesDenylist(resolved) {
		return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: resolved})
	}
	return nil
}

func (fs *Filesystem) matchesDenylist(p string) bool {
	return fs.denylist.MatchesPath(p) || fs.denylist.MatchesPath(normalize(p)) || fs.denylist.MatchesPath(normalize("/"+p))
}

// resolveInRoot returns the path, relative to the server root, that p refers to
// once every symlink in it has been followed the way os.Root follows them.
//
// p itself is cleaned first, as every caller cleans a path before os.Root sees
// it. A link's target is not: os.Root applies ".." in a target to the folder the
// parts before it lead to, so "jump/../config.yml" with "jump -> sub/x" is
// "sub/config.yml". The parts of a target are therefore resolved one at a time,
// and ".." drops the last folder resolved so far, which is never a symlink.
//
// Resolving stops at the first part of the path that does not exist, at an
// absolute symlink (os.Root refuses to follow those) and after maxSymlinkHops
// links; whatever is left of the path is then joined on as it is. ".." at the
// root stays at the root, as with every other path here, and os.Root refuses to
// go above it anyway.
func (fs *Filesystem) resolveInRoot(p string) string {
	r, err := fs.openRoot()
	if err != nil {
		return normalize("/" + p)
	}
	rest := strings.Split(normalize("/"+p), "/")
	var done []string
	hops := 0
	for len(rest) > 0 {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		cur := path.Join(append(done, part)...)
		st, err := r.Lstat(cur)
		if err != nil {
			done = append(append(done, part), rest...)
			break
		}
		if st.Mode()&os.ModeSymlink == 0 {
			done = append(done, part)
			continue
		}
		target, err := r.Readlink(cur)
		hops++
		if err != nil || path.IsAbs(target) || hops > maxSymlinkHops {
			done = append(append(done, part), rest...)
			break
		}
		// Carry on from the folder holding the link, with the target's parts,
		// unresolved, in front of what is left.
		rest = append(strings.Split(target, "/"), rest...)
	}
	return normalize("/" + path.Join(done...))
}
