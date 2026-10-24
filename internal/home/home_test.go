package home

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolver(t *testing.T, goos string, env map[string]string) (Resolver, string) {
	t.Helper()
	user := t.TempDir()
	return Resolver{
		GOOS:        goos,
		Getenv:      func(k string) string { return env[k] },
		UserHomeDir: func() (string, error) { return user, nil },
	}, user
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, data string) {
	t.Helper()
	mkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefault(t *testing.T) {
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want func(user string) string
	}{
		{"linux", "linux", nil, func(u string) string { return filepath.Join(u, ".hermes") }},
		{"linux suffix", "linux", map[string]string{"HERMES_DATA_DIR_SUFFIX": "-dev"}, func(u string) string { return filepath.Join(u, ".hermes-dev") }},
		{"windows localappdata", "windows", map[string]string{"LOCALAPPDATA": "L"}, func(string) string { return filepath.Join("L", "hermes") }},
		{"windows fallback", "windows", nil, func(u string) string { return filepath.Join(u, "AppData", "Local", "hermes") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, user := resolver(t, tt.goos, tt.env)
			got, err := r.Default()
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want(user); got != want {
				t.Fatalf("Default() = %q, want %q", got, want)
			}
		})
	}
}

func TestRootOf(t *testing.T) {
	native := filepath.Join("x", ".hermes")
	tests := []struct {
		name, env, want string
	}{
		{"unset", "", native},
		{"native itself", native, native},
		{"profile under native", filepath.Join(native, "profiles", "work"), native},
		{"custom root", filepath.Join("srv", "h"), filepath.Join("srv", "h")},
		{"custom profile", filepath.Join("srv", "h", "profiles", "a"), filepath.Join("srv", "h")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RootOf(native, tt.env); got != tt.want {
				t.Fatalf("RootOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeProfileName(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"work", "work", false},
		{"Work", "work", false},
		{"a_b-1", "a_b-1", false},
		{"DEFAULT", "default", false},
		{"", "", true},
		{"-x", "", true},
		{"a/b", "", true},
		{"..", "", true},
		{strings.Repeat("a", 65), "", true},
	}
	for _, tt := range tests {
		got, err := NormalizeProfileName(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("NormalizeProfileName(%q) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestResolve(t *testing.T) {
	t.Run("no env no profile uses default", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(user, ".hermes")
		if h != (Home{Root: root, Dir: root, Profile: "default"}) {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("explicit profile is normalized", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		dir := filepath.Join(user, ".hermes", "profiles", "work")
		mkdir(t, dir)
		h, err := r.Resolve("Work")
		if err != nil {
			t.Fatal(err)
		}
		if h.Dir != dir || h.Profile != "work" {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("explicit default maps to root", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		h, err := r.Resolve("default")
		if err != nil {
			t.Fatal(err)
		}
		if h.Dir != filepath.Join(user, ".hermes") {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("explicit beats sticky", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		root := filepath.Join(user, ".hermes")
		mkdir(t, filepath.Join(root, "profiles", "a"))
		mkdir(t, filepath.Join(root, "profiles", "b"))
		write(t, filepath.Join(root, "active_profile"), "b")
		h, err := r.Resolve("a")
		if err != nil {
			t.Fatal(err)
		}
		if h.Profile != "a" {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("missing explicit profile errors", func(t *testing.T) {
		r, _ := resolver(t, "linux", nil)
		if _, err := r.Resolve("ghost"); !errors.Is(err, ErrProfileNotFound) {
			t.Fatalf("err = %v, want ErrProfileNotFound", err)
		}
	})
	t.Run("invalid explicit profile errors", func(t *testing.T) {
		r, _ := resolver(t, "linux", nil)
		if _, err := r.Resolve("../etc"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("sticky active_profile with BOM", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		root := filepath.Join(user, ".hermes")
		mkdir(t, filepath.Join(root, "profiles", "dev"))
		write(t, filepath.Join(root, "active_profile"), "\xEF\xBB\xBFdev\n")
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if h.Profile != "dev" || h.Dir != filepath.Join(root, "profiles", "dev") {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("sticky default is ignored", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		root := filepath.Join(user, ".hermes")
		write(t, filepath.Join(root, "active_profile"), "default\n")
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if h.Dir != root {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("sticky missing errors", func(t *testing.T) {
		r, user := resolver(t, "linux", nil)
		write(t, filepath.Join(user, ".hermes", "active_profile"), "gone")
		if _, err := r.Resolve(""); !errors.Is(err, ErrProfileNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("HERMES_HOME at a profile dir wins over sticky", func(t *testing.T) {
		base := t.TempDir()
		pdir := filepath.Join(base, "profiles", "a")
		mkdir(t, pdir)
		write(t, filepath.Join(base, "active_profile"), "b")
		r, _ := resolver(t, "linux", map[string]string{"HERMES_HOME": pdir})
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if h != (Home{Root: base, Dir: pdir, Profile: "a"}) {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("HERMES_HOME at a root still honours sticky", func(t *testing.T) {
		base := t.TempDir()
		mkdir(t, filepath.Join(base, "profiles", "b"))
		write(t, filepath.Join(base, "active_profile"), "b")
		r, _ := resolver(t, "linux", map[string]string{"HERMES_HOME": base})
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if h.Profile != "b" || h.Root != base {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("HERMES_HOME expands vars and tilde", func(t *testing.T) {
		r, user := resolver(t, "linux", map[string]string{"HERMES_HOME": "~/${SUB}", "SUB": "custom"})
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(user, "custom"); h.Dir != want || h.Root != want {
			t.Fatalf("got %+v, want %q", h, want)
		}
	})
	t.Run("percent vars expand only on windows", func(t *testing.T) {
		env := map[string]string{"HERMES_HOME": "%X%", "X": "y", "LOCALAPPDATA": "L"}
		r, _ := resolver(t, "linux", env)
		h, err := r.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if h.Dir != "%X%" {
			t.Fatalf("linux got %q", h.Dir)
		}
		r, _ = resolver(t, "windows", env)
		if h, err = r.Resolve(""); err != nil || h.Dir != "y" {
			t.Fatalf("windows got %q, %v", h.Dir, err)
		}
	})
}
