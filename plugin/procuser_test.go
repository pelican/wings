package plugin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/pelican/wings/config"
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

// TestCheckReadOnlyToFollowsSymlinkedAncestors: a plugins directory reached
// through a symlink must have the ancestors of its real location checked, or a
// directory the plugin's user can write (here world-writable) slips through.
func TestCheckReadOnlyToFollowsSymlinkedAncestors(t *testing.T) {
	other := ProcessUser{Name: "other", Uid: 2147483000, Gid: 2147483000}
	base := t.TempDir()
	open := filepath.Join(base, "open")
	real := filepath.Join(open, "plugins", "demo")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "run"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(open, "plugins"), filepath.Join(base, "plugins")); err != nil {
		t.Fatal(err)
	}

	err := CheckReadOnlyTo(filepath.Join(base, "plugins", "demo"), other)
	if err == nil || !strings.Contains(err.Error(), "writable by other") || !strings.Contains(err.Error(), open) {
		t.Fatalf("a writable directory on the plugin's real path must be refused, got %v", err)
	}
}

func TestProcessUserIDsRefusesOutOfRange(t *testing.T) {
	if uid, gid, err := (ProcessUser{Name: "pelican", Uid: 988, Gid: 988}).IDs(); err != nil || uid != 988 || gid != 988 {
		t.Fatalf("IDs = %d %d %v", uid, gid, err)
	}
	for _, u := range []ProcessUser{{Name: "neg", Uid: -1, Gid: 0}, {Name: "big", Uid: 0, Gid: 1 << 33}} {
		if _, _, err := u.IDs(); err == nil {
			t.Errorf("%s: an out-of-range id must be refused", u.Name)
		}
	}
}

// TestCheckReachableByRefusesADirectoryTheUserCannotEnter covers a fresh
// install, where Wings creates its root directory with mode 0700: the plugin's
// user can't enter it, so a plugins directory inside it is out of reach and
// exec fails with a bare "permission denied".
func TestCheckReachableByRefusesADirectoryTheUserCannotEnter(t *testing.T) {
	other := ProcessUser{Name: "other", Uid: 2147483000, Gid: 2147483000}
	base := t.TempDir()
	dir := filepath.Join(base, "plugins", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	err := CheckReachableBy(dir, other)
	if err == nil || !strings.Contains(err.Error(), "can't be entered by other") {
		t.Fatalf("a 0700 directory above the plugin must be refused, got %v", err)
	}

	// t.TempDir's directories are 0700.
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	if CheckReachableBy(filepath.Dir(base), other) != nil {
		t.Skip("a directory above the test's temporary directory can't be entered by other users here")
	}
	if err := CheckReachableBy(dir, other); err != nil {
		t.Fatalf("search permission alone is enough to reach the plugin: %v", err)
	}
	// The owner's bits count for the owner, the group's for the group.
	me := ProcessUser{Name: "me", Uid: os.Getuid(), Gid: os.Getgid()}
	if err := os.Chmod(base, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := CheckReachableBy(dir, me); err != nil {
		t.Fatalf("the owner's own search bit lets it in: %v", err)
	}
	// On BSD and macOS a new directory takes its parent's group, not ours.
	if err := os.Chown(base, -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	group := ProcessUser{Name: "grouped", Uid: 2147483000, Gid: os.Getgid()}
	if err := CheckReachableBy(dir, group); err == nil {
		t.Fatal("the group's bits apply to a group member, not the other bits")
	}
}

func TestDefaultPluginsDirectoryIsOutsideTheRootDirectory(t *testing.T) {
	c, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(c.System.RootDirectory, c.Plugins.Directory)
	if err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("plugins.directory %s is inside system.root_directory %s, which Wings creates 0700, so plugins running as system.user can't reach it",
			c.Plugins.Directory, c.System.RootDirectory)
	}
}

// TestPrepareDataDirRefusesASymlinkedDataDirectory: someone who can write the
// plugins directory plants ".data" as a symlink to another tree (/ in the
// real attack). Wings, as root, must not chmod that tree or give a directory
// in it (/etc, for a plugin named "etc") to the plugin's user.
func TestPrepareDataDirRefusesASymlinkedDataDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(filepath.Join(victim, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugins := filepath.Join(base, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(plugins, DataDirName)); err != nil {
		t.Fatal(err)
	}

	u := ProcessUser{Name: strconv.Itoa(65534), Uid: 65534, Gid: 65534, Switch: true}
	err := PrepareDataDir(filepath.Join(plugins, DataDirName, "etc"), u)
	if err == nil {
		t.Error("a symlinked data directory must be refused")
	}
	if mode := statMode(t, victim); mode != 0o755 {
		t.Errorf("the symlink's target was chmodded to %o", mode)
	}
	info, _ := os.Stat(filepath.Join(victim, "etc"))
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 {
		t.Errorf("a directory in the symlink's target was given to uid %d", st.Uid)
	}

	// A plugin's own data directory replaced by a symlink is refused too.
	if err := os.Remove(filepath.Join(plugins, DataDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(plugins, DataDirName), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "etc"), filepath.Join(plugins, DataDirName, "demo")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataDir(filepath.Join(plugins, DataDirName, "demo"), u); err == nil {
		t.Error("a symlinked plugin data directory must be refused")
	}
	info, _ = os.Stat(filepath.Join(victim, "etc"))
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 {
		t.Errorf("the target of a symlinked plugin data directory was given to uid %d", st.Uid)
	}
}

func statMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
