package plugin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestExternalProcessUserIsWingsOwnUserWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("covers Wings running as a normal user")
	}
	u, err := ExternalProcessUser()
	if err != nil {
		t.Fatal(err)
	}
	if u.Switch || u.Uid != os.Geteuid() || u.Name == "" {
		t.Fatalf("user = %+v, want Wings' own user (uid %d) without switching", u, os.Geteuid())
	}
}

func TestUserNameFallsBackToTheUid(t *testing.T) {
	// No system has a user with this uid.
	if got := userName(2147483000); got != "2147483000" {
		t.Fatalf("userName = %q, want the uid", got)
	}
}

// TestCheckReadOnlyTo uses a made-up user, so it runs without root: the
// files belong to whoever runs the test.
func TestCheckReadOnlyTo(t *testing.T) {
	other := ProcessUser{Name: "other", Uid: 2147483000, Gid: 2147483000}
	setup := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "plugins", "demo")
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "run"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	dir := setup(t)
	if err := CheckReadOnlyTo(dir, other); err != nil {
		t.Fatalf("files another user owns, not group or world writable, are read-only to it: %v", err)
	}
	me := ProcessUser{Name: "me", Uid: os.Getuid(), Gid: os.Getgid()}
	if err := CheckReadOnlyTo(dir, me); err == nil || !strings.Contains(err.Error(), "owned by me") {
		t.Fatalf("files the plugin's user owns must be refused, got %v", err)
	}

	dir = setup(t)
	if err := os.Chmod(filepath.Join(dir, "bin", "run"), 0o757); err != nil {
		t.Fatal(err)
	}
	if err := CheckReadOnlyTo(dir, other); err == nil || !strings.Contains(err.Error(), "writable by other") {
		t.Fatalf("a world-writable file must be refused, got %v", err)
	}

	dir = setup(t)
	if err := os.Chmod(filepath.Join(dir, "bin"), 0o775); err != nil {
		t.Fatal(err)
	}
	group := ProcessUser{Name: "grouped", Uid: 2147483000, Gid: os.Getgid()}
	if err := CheckReadOnlyTo(dir, group); err == nil || !strings.Contains(err.Error(), "writable by grouped") {
		t.Fatalf("a directory writable by the plugin's group must be refused, got %v", err)
	}

	dir = setup(t)
	if err := os.Chmod(filepath.Dir(dir), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := CheckReadOnlyTo(dir, other); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a world-writable plugins directory must be refused: the plugin could replace its own directory; got %v", err)
	}
}

// TestPrepareDataDirSkipsHardLinks covers a plugin that hard links a file it
// doesn't own (/etc/shadow, say) into its data directory: Wings, as root,
// must not give that file to the plugin's user.
func TestPrepareDataDirSkipsHardLinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "plugins", DataDirName, "demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "own"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "symlinked")); err != nil {
		t.Fatal(err)
	}

	u := ProcessUser{Name: strconv.Itoa(65534), Uid: 65534, Gid: 65534, Switch: true}
	if err := PrepareDataDir(dir, u); err != nil {
		t.Fatal(err)
	}
	owner := func(p string) uint32 {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Sys().(*syscall.Stat_t).Uid
	}
	if owner(secret) != 0 {
		t.Fatal("a hard-linked file was given to the plugin's user")
	}
	if owner(filepath.Join(dir, "own")) != 65534 || owner(dir) != 65534 {
		t.Fatal("the data directory and its own files should belong to the plugin's user")
	}
}
