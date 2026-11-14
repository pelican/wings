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

func (ts *TrustStore) save() error {
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ts.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ts.path, b, 0o600)
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
	ts.Approved[m.Name] = fp
	return ts.save()
}

// Revoke removes approval for a plugin name and persists the change.
func (ts *TrustStore) Revoke(name string) error {
	delete(ts.Approved, name)
	return ts.save()
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
	h := sha256.New()
	write := func(s string) { io.WriteString(h, s); h.Write([]byte{0}) }

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
			write("symlink")
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			write(target)
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
