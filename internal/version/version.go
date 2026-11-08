package version

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

var (
	// Version is the current authoritative semver version of Flitnix.
	Version = "0.2.7.3"

	// Prerelease indicates a release stage like "beta.1", "rc.1", or "dev".
	// Set to empty string for official stable releases.
	Prerelease = ""

	// GitCommit is set at compile time via -ldflags "-X github.com/itsyamin/flitnix/internal/version.GitCommit=...".
	GitCommit = "unknown"

	// BuildDate is set at compile time via -ldflags "-X github.com/itsyamin/flitnix/internal/version.BuildDate=...".
	BuildDate = "unknown"
)

// Info encapsulates the complete build and version metadata.
type Info struct {
	Version      string `json:"version"`
	Prerelease   string `json:"prerelease,omitempty"`
	GitCommit    string `json:"gitCommit,omitempty"`
	BuildDate    string `json:"buildDate,omitempty"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	FullString   string `json:"fullString"`
	IsDev        bool   `json:"isDev"`
}

// Get returns the authoritative build version metadata.
func Get() Info {
	v := Version
	if Prerelease != "" {
		v = fmt.Sprintf("%s-%s", Version, Prerelease)
	}

	isDev := Prerelease == "dev" || GitCommit == "unknown" || strings.Contains(Prerelease, "dev")

	return Info{
		Version:      Version,
		Prerelease:   Prerelease,
		GitCommit:    GitCommit,
		BuildDate:    BuildDate,
		Platform:     runtime.GOOS,
		Architecture: runtime.GOARCH,
		FullString:   fmt.Sprintf("Flitnix v%s (%s/%s)", v, runtime.GOOS, runtime.GOARCH),
		IsDev:        isDev,
	}
}

// String returns the normalized version string (e.g. "v2.7.0").
func String() string {
	if Prerelease != "" {
		return fmt.Sprintf("v%s-%s", Version, Prerelease)
	}
	return "v" + Version
}

var semverPattern = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)

// SemVer represents a parsed semantic version.
type SemVer struct {
	Major      int
	Minor      int
	Patch      int
	Build      int
	Prerelease string
}

// ParseSemVer parses a version string like "v0.2.7.3", "v2.7.0", "2.6", or "2.6.0-rc1".
func ParseSemVer(v string) (SemVer, error) {
	v = strings.TrimSpace(v)
	matches := semverPattern.FindStringSubmatch(v)
	if matches == nil {
		return SemVer{}, fmt.Errorf("invalid semver: %q", v)
	}

	major, _ := strconv.Atoi(matches[1])
	minor := 0
	if matches[2] != "" {
		minor, _ = strconv.Atoi(matches[2])
	}
	patch := 0
	if matches[3] != "" {
		patch, _ = strconv.Atoi(matches[3])
	}
	build := 0
	if matches[4] != "" {
		build, _ = strconv.Atoi(matches[4])
	}

	return SemVer{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Build:      build,
		Prerelease: matches[5],
	}, nil
}

// Compare compares two version strings.
// Returns:
//   -1 if v1 < v2
//    0 if v1 == v2
//    1 if v1 > v2
func Compare(v1, v2 string) (int, error) {
	s1, err := ParseSemVer(v1)
	if err != nil {
		return 0, err
	}
	s2, err := ParseSemVer(v2)
	if err != nil {
		return 0, err
	}

	if s1.Major != s2.Major {
		if s1.Major > s2.Major {
			return 1, nil
		}
		return -1, nil
	}
	if s1.Minor != s2.Minor {
		if s1.Minor > s2.Minor {
			return 1, nil
		}
		return -1, nil
	}
	if s1.Patch != s2.Patch {
		if s1.Patch > s2.Patch {
			return 1, nil
		}
		return -1, nil
	}
	if s1.Build != s2.Build {
		if s1.Build > s2.Build {
			return 1, nil
		}
		return -1, nil
	}

	// When major, minor, patch, build are equal:
	// A version without a prerelease is higher than one with a prerelease (e.g. 2.6.0 > 2.6.0-rc1).
	if s1.Prerelease == "" && s2.Prerelease != "" {
		return 1, nil
	}
	if s1.Prerelease != "" && s2.Prerelease == "" {
		return -1, nil
	}
	if s1.Prerelease != s2.Prerelease {
		if s1.Prerelease > s2.Prerelease {
			return 1, nil
		}
		return -1, nil
	}

	return 0, nil
}

// IsNewer returns true if candidate is strictly newer than baseline.
func IsNewer(baseline, candidate string) bool {
	res, err := Compare(candidate, baseline)
	if err != nil {
		return false
	}
	return res > 0
}
