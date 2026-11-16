package plugin

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/pelican/wings/config"
)

// TestSettingsBelongToThePluginsUser covers settings saved while the plugin
// runs as system.user: it reads settings.json when the file changes, so the
// file Wings (root) writes must be readable by that user.
func TestSettingsBelongToThePluginsUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: only root changes the plugin's user")
	}
	c, err := config.NewAtPath("test.yml")
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.System.User.Uid, c.System.User.Gid = 65534, 65534
	config.Set(c)

	m := writeSettingsManifest(t, settingsManifest)
	if err := SaveSettings(m, map[string]any{"webhook_url": "https://example.test/hook"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(m.DataDir(), SettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 65534 || st.Gid != 65534 || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json is %d:%d mode %o, want 65534:65534 mode 600", st.Uid, st.Gid, info.Mode().Perm())
	}
}

// TestLoadSettingsReadsOnlyARegularFileInTheDataDirectory covers a plugin,
// which owns its data directory, putting something other than a file there
// for Wings to read: a FIFO would block the read forever, and a symlink could
// show Wings' own files in the plugins directory as the plugin's settings.
func TestLoadSettingsReadsOnlyARegularFileInTheDataDirectory(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		m := writeSettingsManifest(t, settingsManifest)
		if err := os.MkdirAll(m.DataDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(m.DataDir(), SettingsFile), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := LoadSettings(m)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a FIFO was read as settings")
			}
		case <-time.After(2 * time.Second):
			// Unblock the reader so the test can end.
			if f, err := os.OpenFile(filepath.Join(m.DataDir(), SettingsFile), os.O_WRONLY, 0); err == nil {
				f.Close()
			}
			t.Fatal("reading a FIFO named settings.json blocked")
		}
	})
	t.Run("symlink out of the data directory", func(t *testing.T) {
		m := writeSettingsManifest(t, settingsManifest)
		if err := os.MkdirAll(m.DataDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		// A file of Wings' in the plugins directory, outside the data directory.
		other := filepath.Join(filepath.Dir(m.Dir()), ".trust.json")
		if err := os.WriteFile(other, []byte(`{"webhook_url":"from the trust store"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", ".trust.json"), filepath.Join(m.DataDir(), SettingsFile)); err != nil {
			t.Fatal(err)
		}
		got, err := LoadSettings(m)
		if err == nil && got["webhook_url"] != nil {
			t.Fatalf("settings were read through a symlink out of the data directory: %v", got)
		}
	})
}
