// Package home resolves the Hermes home directory and profiles the same way
// upstream Hermes Agent does (hermes_constants.py, hermes_cli/profiles.py).
package home

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrProfileNotFound reports a selected profile whose directory is missing.
var ErrProfileNotFound = errors.New("profile not found")

var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// DefaultProfile is the name of the profile that lives at the root.
const DefaultProfile = "default"

// Resolver holds the process facts that home resolution reads, so tests can
// supply their own instead of touching the real environment.
type Resolver struct {
	GOOS        string
	Getenv      func(string) string
	UserHomeDir func() (string, error)
}

// Home is a resolved Hermes home.
type Home struct {
	// Root holds profiles/ and active_profile.
	Root string
	// Dir is the active profile's directory (Root for the default profile).
	Dir string
	// Profile is the normalized profile name.
	Profile string
}

// FromOS builds a Resolver backed by the real process.
func FromOS(goos string) Resolver {
	return Resolver{GOOS: goos, Getenv: os.Getenv, UserHomeDir: os.UserHomeDir}
}

// Default returns the platform default home, ignoring HERMES_HOME.
func (r Resolver) Default() (string, error) {
	suffix := r.Getenv("HERMES_DATA_DIR_SUFFIX")
	if r.GOOS == "windows" {
		if local := r.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "hermes"+suffix), nil
		}
		user, err := r.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		return filepath.Join(user, "AppData", "Local", "hermes"+suffix), nil
	}
	user, err := r.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(user, ".hermes"+suffix), nil
}

// RootOf returns the root that holds envPath: the native home when envPath is
// inside it, the grandparent of a <root>/profiles/<name> path, or envPath.
func RootOf(native, envPath string) string {
	if envPath == "" || within(native, envPath) {
		return native
	}
	if filepath.Base(filepath.Dir(envPath)) == "profiles" {
		return filepath.Dir(filepath.Dir(envPath))
	}
	return envPath
}

func within(base, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// NormalizeProfileName lowercases and validates a profile name.
func NormalizeProfileName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == DefaultProfile {
		return n, nil
	}
	if !profileNameRE.MatchString(n) {
		return "", fmt.Errorf("invalid profile name %q: must match %s", name, profileNameRE)
	}
	return n, nil
}

// Resolve picks the home for this process. An explicit profile wins; then a
// HERMES_HOME that points at a profiles/<name> dir; then the sticky
// <root>/active_profile; then HERMES_HOME or the platform default.
func (r Resolver) Resolve(profile string) (Home, error) {
	native, err := r.Default()
	if err != nil {
		return Home{}, err
	}
	envPath, err := r.expand(r.Getenv("HERMES_HOME"))
	if err != nil {
		return Home{}, err
	}
	root := RootOf(native, envPath)
	base := native
	if envPath != "" {
		base = envPath
	}

	if profile == "" && envPath != "" && filepath.Base(filepath.Dir(envPath)) == "profiles" {
		return Home{Root: root, Dir: envPath, Profile: filepath.Base(envPath)}, nil
	}

	sticky := false
	if profile == "" {
		profile = readActiveProfile(filepath.Join(root, "active_profile"))
		sticky = profile != ""
	}
	if profile == "" {
		return Home{Root: root, Dir: base, Profile: DefaultProfile}, nil
	}

	name, err := NormalizeProfileName(profile)
	if err != nil {
		return Home{}, err
	}
	if name == DefaultProfile {
		return Home{Root: root, Dir: root, Profile: name}, nil
	}
	dir := filepath.Join(root, "profiles", name)
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		if sticky {
			return Home{}, fmt.Errorf("saved profile %q no longer exists, switch back with `hermes profile use default`: %w", name, ErrProfileNotFound)
		}
		return Home{}, fmt.Errorf("profile %q at %s: %w", name, dir, ErrProfileNotFound)
	}
	return Home{Root: root, Dir: dir, Profile: name}, nil
}

// readActiveProfile returns the sticky profile name, or "" when the file is
// missing, unreadable, or names the default profile.
func readActiveProfile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))))
	if strings.EqualFold(name, DefaultProfile) {
		return ""
	}
	return name
}

var envRefRE = regexp.MustCompile(`%([^%]+)%|\$\{([^}]+)\}|\$([A-Za-z0-9_]+)`)

// expand mirrors os.path.expandvars + expanduser: undefined references stay
// verbatim, %NAME% only expands on Windows, and a leading ~ becomes the user
// home.
func (r Resolver) expand(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	p = envRefRE.ReplaceAllStringFunc(p, func(m string) string {
		sub := envRefRE.FindStringSubmatch(m)
		if sub[1] != "" && r.GOOS != "windows" {
			return m
		}
		if v := r.Getenv(sub[1] + sub[2] + sub[3]); v != "" {
			return v
		}
		return m
	})
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		user, err := r.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand HERMES_HOME: %w", err)
		}
		p = filepath.Join(user, p[1:])
	}
	return filepath.Clean(p), nil
}
