package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/pelican/wings/config"
)

const trustFileName = ".trust.json"

// TrustStore records which external plugins the operator has explicitly approved
// to run. A plugin is keyed by name to a fingerprint of its security-relevant
// manifest fields. If those change, the fingerprint no longer matches and the
// plugin must be re-approved: a plugin cannot silently change what is executed,
// what host access it asks for, or what runtime scope it registers for. The
// daemon loads only trusted external plugins.
type TrustStore struct {
	path     string
	Approved map[string]string `json:"approved"`
}

func trustPath() string {
	return filepath.Join(config.Get().Plugins.Directory, trustFileName)
}

// LoadTrustStore reads the trust file, returning an empty store if none exists.
func LoadTrustStore() (*TrustStore, error) {
	path := trustPath()
	ts := &TrustStore{path: path, Approved: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ts, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, ts); err != nil {
		return nil, err
	}
	if ts.Approved == nil {
		ts.Approved = map[string]string{}
	}
	ts.path = path
	return ts, nil
}

// update applies fn to the approvals as they are on disk now, under an
// exclusive lock, and writes the result back atomically. Two CLI runs (say
// `trust a` and `untrust b`) each loaded the store earlier; without the reload
// under the lock, the later save would restore whatever the other one removed.
func (ts *TrustStore) update(fn func(approved map[string]string)) error {
	if err := os.MkdirAll(filepath.Dir(ts.path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(ts.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("plugin: locking trust store: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	current := &TrustStore{Approved: map[string]string{}}
	if b, err := os.ReadFile(ts.path); err == nil {
		if err := json.Unmarshal(b, current); err != nil {
			return err
		}
		if current.Approved == nil {
			current.Approved = map[string]string{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	fn(current.Approved)

	b, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ts.path), ".trust-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), ts.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	ts.Approved = current.Approved
	return nil
}

// IsTrusted reports whether the manifest has been approved with a fingerprint
// matching its current security-relevant fields.
func (ts *TrustStore) IsTrusted(m Manifest) bool {
	fp, err := fingerprint(m)
	if err != nil {
		return false
	}
	return ts.Approved[m.Name] == fp
}

// Approve records approval for the manifest's current fingerprint and persists it.
func (ts *TrustStore) Approve(m Manifest) error {
	fp, err := fingerprint(m)
	if err != nil {
		return err
	}
	return ts.update(func(approved map[string]string) { approved[m.Name] = fp })
}

// Revoke removes approval for a plugin name and persists the change.
func (ts *TrustStore) Revoke(name string) error {
	return ts.update(func(approved map[string]string) { delete(approved, name) })
}

// fingerprint hashes the fields that determine what a plugin runs, what it asks
// for, and what runtime surface it registers for. Any change to those
// invalidates a prior approval.
func fingerprint(m Manifest) (string, error) {
	h := sha256.New()
	write := func(s string) { io.WriteString(h, s); h.Write([]byte{0}) }

	writeField := func(label, value string) {
		write(label)
		write(value)
	}
	writeList := func(label string, values []string, sorted bool) {
		write(label)
		items := append([]string(nil), values...)
		if sorted {
			sort.Strings(items)
		}
		for _, value := range items {
			write(value)
		}
	}

	writeField("name", m.Name)
	writeField("version", m.Version)
	writeField("capability", string(m.Capability))
	writeList("entrypoint", m.Entrypoint, false)
	writeList("provides.events", EffectiveEvents(m), true)
	writeList("requires.hostAPI", m.Requires.HostAPI, true)
	content, err := contentDigest(m.Dir())
	if err != nil {
		return "", err
	}
	writeField("content", content)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func contentDigest(root string) (string, error) {
	if root == "" {
		return "", nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	write := func(s string) { io.WriteString(h, s); h.Write([]byte{0}) }

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		write(rel)
		mode := info.Mode()
		switch {
		case d.IsDir():
			write("dir")
		case mode.Type() == 0:
			write(fmt.Sprintf("file:%o", mode.Perm()))
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			fh := sha256.New()
			_, copyErr := io.Copy(fh, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			write(hex.EncodeToString(fh.Sum(nil)))
		case mode&os.ModeSymlink != 0:
			// Hashing only the link text would let the file it points to
			// change without a new approval. A link is allowed only when it
			// resolves inside the plugin directory, where the walk hashes the
			// target itself (node_modules/.bin links, for example).
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("plugin: resolving symlink %s: %w", rel, err)
			}
			inside, err := filepath.Rel(realRoot, resolved)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return fmt.Errorf("plugin: symlink %s points outside the plugin directory", rel)
			}
			write("symlink")
			write(filepath.ToSlash(inside))
		default:
			return fmt.Errorf("plugin: unsupported file type in plugin directory: %s", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
