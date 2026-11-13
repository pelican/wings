package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pelican/wings/config"
)

// depsDirName is where a plugin's dependency source is laid out, inside the
// plugin directory so that uninstalling a plugin takes its libraries with it.
//
// It is a dotfile, which keeps it out of the way of the two things that walk a
// plugin directory: source loading only reads top-level .go files, and plugin
// discovery only looks at the plugin root.
const depsDirName = ".deps"

// modulePathPattern is what a module path may contain. Anything else is
// refused rather than sanitised, since a module path becomes a directory path
// and the proxy URL.
var modulePathPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~/-]{0,200}$`)

// versionPattern is the exact-version form accepted in a manifest. A range
// would mean one plugin behaving differently on two nodes.
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+incompatible)?$`)

// requirePattern matches a require line in a go.mod, in either the single-line
// or block form, which is how transitive dependencies are discovered without a
// Go toolchain.
var requirePattern = regexp.MustCompile(`^\s*(?:require\s+)?([a-zA-Z0-9][^\s]*)\s+(v[^\s]+)`)

// moduleRequest is one module to fetch.
type moduleRequest struct {
	Path    string
	Version string
}

func (m moduleRequest) String() string { return m.Path + "@" + m.Version }

// dependencyFetcher resolves a plugin's declared modules into a GOPATH the
// interpreter can import from.
//
// There is deliberately no Go toolchain involved. Modules come from the proxy
// as source archives over plain HTTP and are unpacked into a GOPATH layout,
// which is all the interpreter needs to resolve an import. That is what keeps
// the "no build step, works in a distroless image" property intact while still
// letting a plugin use a library.
type dependencyFetcher struct {
	proxy        string
	maxBytes     int64
	maxModules   int
	client       *http.Client
	pinnedHashes map[string]string
}

func newDependencyFetcher(cfg config.PluginConfiguration, pinned map[string]string) *dependencyFetcher {
	proxy := strings.TrimSuffix(strings.TrimSpace(cfg.ModuleProxy), "/")
	if proxy == "" {
		proxy = "https://proxy.golang.org"
	}

	maxBytes := cfg.MaxDependencySize * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 32 * 1024 * 1024
	}

	maxModules := cfg.MaxDependencies
	if maxModules <= 0 {
		maxModules = 50
	}

	return &dependencyFetcher{
		proxy:        proxy,
		maxBytes:     maxBytes,
		maxModules:   maxModules,
		pinnedHashes: pinned,
		client:       &http.Client{Timeout: 2 * time.Minute},
	}
}

// ensureDependencies lays out a plugin's modules and returns the GOPATH to
// interpret against. An empty return means the plugin declared none.
//
// Already-present modules at the right version are left alone, so enabling a
// plugin a second time costs nothing. Anything unexpected is an error rather
// than a partial layout, because a plugin missing one of its libraries fails
// in a far more confusing way than one that refuses to load.
func (m *Manager) ensureDependencies(inst *Instance) (string, error) {
	manifest := inst.Manifest()

	if len(manifest.Dependencies) == 0 {
		return "", nil
	}

	if !m.cfg.AllowDependencies {
		return "", errors.Errorf(
			"plugins: %s declares %d Go module dependencies, but allow_dependencies is off for this node",
			manifest.ID, len(manifest.Dependencies),
		)
	}

	root := filepath.Join(manifest.Directory(), depsDirName)
	src := filepath.Join(root, "src")

	if err := os.MkdirAll(src, 0o700); err != nil {
		return "", errors.Wrap(err, "plugins: could not create the dependency directory")
	}

	fetcher := newDependencyFetcher(m.cfg, manifest.DependencyHashes)

	// Start from what the plugin declared, then follow each module's own
	// requirements. Highest version wins when two modules disagree, which is
	// the same rule the Go toolchain's minimal version selection lands on for
	// the simple cases a plugin is likely to hit.
	wanted := map[string]string{}
	for path, version := range manifest.Dependencies {
		path = strings.TrimSpace(path)
		version = strings.TrimSpace(version)

		if !modulePathPattern.MatchString(path) {
			return "", errors.Errorf("plugins: %q is not a usable module path", path)
		}
		if !versionPattern.MatchString(version) {
			return "", errors.Errorf(
				"plugins: %s needs an exact version such as v1.2.3, but %s declares %q",
				path, manifest.ID, version,
			)
		}
		wanted[path] = version
	}

	// Resolution follows what the source actually imports, not everything the
	// modules happen to require.
	//
	// A go.mod lists every module its own tests and tooling need, which for a
	// small library routinely means tens of megabytes of unrelated source: the
	// first version of this followed those requires and pulled the whole of
	// x/text in to use a semver helper. The Go toolchain avoids that by
	// pruning the module graph against real imports, and so does this, by
	// reading the import statements out of each module once it is on disk and
	// fetching only what they name.
	resolved := map[string]bool{}
	queue := make([]moduleRequest, 0, len(wanted))

	for path, version := range wanted {
		queue = append(queue, moduleRequest{Path: path, Version: version})
	}
	// Sorted so a failure is reported against the same module every time
	// rather than depending on map order.
	sort.Slice(queue, func(a, b int) bool { return queue[a].Path < queue[b].Path })

	for len(queue) > 0 {
		mod := queue[0]
		queue = queue[1:]

		if resolved[mod.Path] {
			continue
		}
		resolved[mod.Path] = true

		if len(resolved) > fetcher.maxModules {
			return "", errors.Errorf(
				"plugins: %s pulls in more than %d modules, which is past this node's limit",
				manifest.ID, fetcher.maxModules,
			)
		}

		requires, err := fetcher.ensure(src, mod)
		if err != nil {
			return "", err
		}

		// What this module's own source imports, minus the standard library
		// and anything already laid out.
		imports, err := collectImports(filepath.Join(src, filepath.FromSlash(mod.Path)))
		if err != nil {
			return "", err
		}

		for _, imported := range imports {
			if isStandardLibrary(imported) || coveredBy(imported, resolved) {
				continue
			}

			// Find which of this module's requires owns the imported package.
			// The longest matching module path wins, the way Go resolves a
			// package to the most specific module that could contain it.
			owner, ok := ownerOf(imported, requires)
			if !ok {
				// Nothing declares it. Left alone deliberately: the import
				// error the interpreter raises names the package and is far
				// more useful than a guess at which module to fetch.
				continue
			}

			if current, seen := wanted[owner.Path]; seen && compareModuleVersions(owner.Version, current) <= 0 {
				continue
			}
			wanted[owner.Path] = owner.Version
			queue = append(queue, owner)
		}
	}

	m.log().WithFields(log.Fields{
		"plugin":  manifest.ID,
		"modules": len(resolved),
		"gopath":  root,
	}).Info("laid out a plugin's Go module dependencies")

	return root, nil
}

// ensure downloads and extracts one module unless it is already present, and
// returns the modules it requires in turn.
func (f *dependencyFetcher) ensure(src string, mod moduleRequest) ([]moduleRequest, error) {
	target := filepath.Join(src, filepath.FromSlash(mod.Path))
	stamp := filepath.Join(target, ".version")

	// Already laid out at this exact version: nothing to do.
	if existing, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(existing)) == mod.Version {
		if raw, err := os.ReadFile(filepath.Join(target, "go.mod")); err == nil {
			return parseGoModRequires(string(raw)), nil
		}
		return nil, nil
	}

	zipBytes, err := f.download(mod, "zip")
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(zipBytes)
	actual := hex.EncodeToString(sum[:])

	if pinned, ok := f.pinnedHashes[mod.String()]; ok {
		if !strings.EqualFold(strings.TrimSpace(pinned), actual) {
			return nil, errors.Errorf(
				"plugins: %s does not match the hash pinned for it (expected %s, got %s)",
				mod, pinned, actual,
			)
		}
	} else {
		// Not pinned, so say exactly what was fetched. An author reading this
		// can paste it into dependency_hashes and have the node refuse a
		// substitute afterwards.
		log.WithFields(log.Fields{
			"subsystem": "plugins",
			"module":    mod.String(),
			"sha256":    actual,
		}).Info("fetched an unpinned plugin dependency")
	}

	// Replace rather than merge, so a version change cannot leave files from
	// the previous one behind.
	if err := os.RemoveAll(target); err != nil {
		return nil, errors.Wrapf(err, "plugins: could not clear %s", target)
	}

	if err := f.extract(zipBytes, mod, target); err != nil {
		return nil, err
	}

	if err := os.WriteFile(stamp, []byte(mod.Version), 0o600); err != nil {
		return nil, errors.Wrap(err, "plugins: could not record the module version")
	}

	// The archive's go.mod is the only place a module's own requirements are
	// written down, and reading it is how this resolves a dependency tree with
	// no toolchain available.
	raw, err := os.ReadFile(filepath.Join(target, "go.mod"))
	if err != nil {
		return nil, nil
	}
	return parseGoModRequires(string(raw)), nil
}

// download fetches one file for a module from the proxy.
func (f *dependencyFetcher) download(mod moduleRequest, kind string) ([]byte, error) {
	escapedPath, err := escapeModuleElement(mod.Path)
	if err != nil {
		return nil, err
	}
	escapedVersion, err := escapeModuleElement(mod.Version)
	if err != nil {
		return nil, err
	}

	url := f.proxy + "/" + escapedPath + "/@v/" + escapedVersion + "." + kind

	res, err := f.client.Get(url)
	if err != nil {
		return nil, errors.Wrapf(err, "plugins: could not reach the module proxy for %s", mod)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
		return nil, errors.Errorf("plugins: the module proxy has no %s", mod)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errors.Errorf("plugins: the module proxy returned HTTP %d for %s", res.StatusCode, mod)
	}

	// Read one byte past the cap so an oversized archive is refused rather
	// than silently truncated into something that will not unzip.
	content, err := io.ReadAll(io.LimitReader(res.Body, f.maxBytes+1))
	if err != nil {
		return nil, errors.Wrapf(err, "plugins: could not download %s", mod)
	}
	if int64(len(content)) > f.maxBytes {
		return nil, errors.Errorf(
			"plugins: %s is larger than the %d MiB limit for a plugin dependency",
			mod, f.maxBytes/(1024*1024),
		)
	}

	return content, nil
}

// extract unpacks a module archive into a GOPATH source directory.
//
// Proxy archives name every entry "<module>@<version>/...", which is stripped
// so the result is the plain "src/<module path>" layout an import resolves
// against.
func (f *dependencyFetcher) extract(archive []byte, mod moduleRequest, target string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return errors.Wrapf(err, "plugins: %s is not a readable module archive", mod)
	}

	prefix := mod.Path + "@" + mod.Version + "/"

	var written int64

	for _, file := range zr.File {
		name := strings.TrimPrefix(file.Name, prefix)
		if name == file.Name || name == "" || strings.HasSuffix(name, "/") {
			continue
		}

		// Only source and the module file are kept. Tests, testdata and
		// vendored trees are the bulk of most archives and none of it is
		// reachable from interpreted code.
		if !keepModuleFile(name) {
			continue
		}

		if strings.Contains(name, "..") {
			return errors.Errorf("plugins: %s contains an unsafe path: %s", mod, file.Name)
		}

		dest := filepath.Join(target, filepath.FromSlash(name))
		if !strings.HasPrefix(dest, filepath.Clean(target)+string(os.PathSeparator)) {
			return errors.Errorf("plugins: %s would write outside its own directory", mod)
		}

		written += int64(file.UncompressedSize64)
		if written > f.maxBytes {
			return errors.Errorf("plugins: %s expands past the %d MiB limit", mod, f.maxBytes/(1024*1024))
		}

		if err := writeModuleFile(file, dest); err != nil {
			return errors.Wrapf(err, "plugins: could not extract %s from %s", name, mod)
		}
	}

	return nil
}

// keepModuleFile reports whether an archive entry is worth writing to disk.
func keepModuleFile(name string) bool {
	if name == "go.mod" {
		return true
	}
	if !strings.HasSuffix(name, ".go") {
		return false
	}

	// A module's own tests are never reached from interpreted code, and they
	// routinely import packages plugins are not given, which would otherwise
	// look like a broken dependency.
	if strings.HasSuffix(name, "_test.go") {
		return false
	}

	// testdata is never imported, and a vendored tree would shadow the
	// modules laid out alongside it.
	for _, part := range strings.Split(name, "/") {
		if part == "testdata" || part == "vendor" {
			return false
		}
	}
	return true
}

func writeModuleFile(file *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}

	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.CopyN(out, rc, int64(file.UncompressedSize64))
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// parseGoModRequires pulls the require lines out of a go.mod.
//
// This is a deliberately small parser rather than a dependency on the module
// tooling: all that is needed is module paths and versions, and the format of
// a require line is stable. Anything it cannot read is skipped, and a missing
// transitive dependency surfaces as a clear import error when the plugin
// loads.
func parseGoModRequires(content string) []moduleRequest {
	var out []moduleRequest
	inBlock := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)

		if idx := strings.Index(trimmed, "//"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if trimmed == "" {
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		case !inBlock && !strings.HasPrefix(trimmed, "require "):
			continue
		}

		if m := requirePattern.FindStringSubmatch(trimmed); m != nil {
			out = append(out, moduleRequest{Path: m[1], Version: m[2]})
		}
	}

	return out
}

// escapeModuleElement applies the module proxy's case encoding, where an
// uppercase letter becomes "!" plus its lowercase form. Without it a module
// with capitals in its path cannot be fetched.
func escapeModuleElement(s string) (string, error) {
	if s == "" {
		return "", errors.New("plugins: empty module path element")
	}

	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// compareModuleVersions orders two module versions, returning a negative
// number when a sorts before b. Prerelease ordering is approximated: a
// prerelease sorts before the release it leads to, which is the part that
// matters for picking the higher of two requirements.
func compareModuleVersions(a, b string) int {
	an, apre := splitModuleVersion(a)
	bn, bpre := splitModuleVersion(b)

	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			return an[i] - bn[i]
		}
	}

	switch {
	case apre == "" && bpre == "":
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return strings.Compare(apre, bpre)
}

func splitModuleVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSuffix(v, "+incompatible"), "v")

	var pre string
	if idx := strings.Index(v, "-"); idx >= 0 {
		pre = v[idx+1:]
		v = v[:idx]
	}

	var nums [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		nums[i], _ = strconv.Atoi(part)
	}
	return nums, pre
}

// collectImports returns every package a module's source imports, read with
// the real Go parser rather than by pattern matching, so an import block,
// a grouped import and an aliased one are all read correctly.
func collectImports(dir string) ([]string, error) {
	seen := map[string]bool{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()

		// ImportsOnly stops at the import block, so this does not pay to
		// parse the body of every file in a module.
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			// A file this parser cannot read is one the interpreter will not
			// be able to read either, and it will say so with the position.
			// Skipping it here keeps resolution going for the rest.
			return nil
		}

		for _, spec := range file.Imports {
			imported, uErr := strconv.Unquote(spec.Path.Value)
			if uErr != nil {
				continue
			}
			seen[imported] = true
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "plugins: could not read the imports under %s", dir)
	}

	out := make([]string, 0, len(seen))
	for imported := range seen {
		out = append(out, imported)
	}
	sort.Strings(out)
	return out, nil
}

// isStandardLibrary reports whether an import path belongs to the standard
// library.
//
// The test is whether the first path element contains a dot. Every module path
// starts with a domain and no standard library package does, which is the same
// rule the Go toolchain itself uses to tell them apart.
func isStandardLibrary(importPath string) bool {
	first := importPath
	if idx := strings.Index(importPath, "/"); idx >= 0 {
		first = importPath[:idx]
	}
	return !strings.Contains(first, ".")
}

// coveredBy reports whether an import is already satisfied by a module that
// has been laid out.
func coveredBy(importPath string, resolved map[string]bool) bool {
	for path := range resolved {
		if importPath == path || strings.HasPrefix(importPath, path+"/") {
			return true
		}
	}
	return false
}

// ownerOf picks the module that would contain an imported package, preferring
// the longest matching path because a more specific module is the one that
// actually holds it.
func ownerOf(importPath string, candidates []moduleRequest) (moduleRequest, bool) {
	var best moduleRequest
	found := false

	for _, candidate := range candidates {
		if importPath != candidate.Path && !strings.HasPrefix(importPath, candidate.Path+"/") {
			continue
		}
		if !modulePathPattern.MatchString(candidate.Path) || !versionPattern.MatchString(candidate.Version) {
			continue
		}
		if !found || len(candidate.Path) > len(best.Path) {
			best = candidate
			found = true
		}
	}

	return best, found
}
