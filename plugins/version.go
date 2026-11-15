package plugins

import (
	"regexp"
	"strconv"
	"strings"
)

// ManifestName is the file that marks a directory as a plugin.
const ManifestName = "plugin.json"

// semverPattern matches a leading major.minor.patch, ignoring any prerelease
// or build metadata that follows.
var semverPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// comparableVersion reduces a Wings version string to a bare major.minor.patch
// that can be compared against a manifest constraint.
//
// It returns an empty string for anything that is not a release version, which
// covers both the "develop" default and the "dev-<sha>" builds CI produces.
// Callers treat that as "cannot tell" rather than as "does not match", because
// refusing every plugin on a development build would make plugins impossible to
// develop against.
func comparableVersion(v string) string {
	m := semverPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2] + "." + m[3]
}

// normalizeVersion pads a partial version such as "1.2" out to "1.2.0" so it
// can be compared against a full one.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "^"))
	if v == "" {
		return "0.0.0"
	}
	parts := strings.SplitN(v, "-", 2)
	nums := strings.Split(parts[0], ".")
	for len(nums) < 3 {
		nums = append(nums, "0")
	}
	return strings.Join(nums[:3], ".")
}

// compareVersions orders two major.minor.patch strings, returning a negative
// number when a sorts before b, zero when they are equal, and a positive
// number otherwise.
func compareVersions(a, b string) int {
	as := strings.Split(normalizeVersion(a), ".")
	bs := strings.Split(normalizeVersion(b), ".")

	for i := 0; i < 3; i++ {
		ai, _ := strconv.Atoi(as[i])
		bi, _ := strconv.Atoi(bs[i])
		if ai != bi {
			return ai - bi
		}
	}
	return 0
}
