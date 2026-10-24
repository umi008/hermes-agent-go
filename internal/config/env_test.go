package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	tests := []struct {
		name, line, key, want string
	}{
		{"plain", "A=b", "A", "b"},
		{"export prefix", "export A=b", "A", "b"},
		{"spaces around key", "  A  = b ", "A", "b"},
		{"inline comment", "A=b # note", "A", "b"},
		{"hash without space is data", "A=b#c", "A", "b#c"},
		{"double quoted", `A="b c"`, "A", "b c"},
		{"double quoted escapes", `A="say \"hi\" \\ \n"`, "A", `say "hi" \ \n`},
		{"double quoted comment", `A="b # c" # real`, "A", "b # c"},
		{"single quoted", `A='b \" c'`, "A", `b \" c`},
		{"single quoted comment", `A='x' # c`, "A", "x"},
		{"unterminated quote kept", `A="abc # x`, "A", `"abc # x`},
		{"equals in value", "A=b=c", "A", "b=c"},
		{"empty value", "A=", "A", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDotenv([]byte(tt.line))
			if v, ok := got[tt.key]; !ok || v != tt.want {
				t.Fatalf("got %q (present=%v), want %q", v, ok, tt.want)
			}
		})
	}
}

func TestParseDotenvSkips(t *testing.T) {
	got := ParseDotenv([]byte("\xEF\xBB\xBF# comment\n\nnovalue\n=x\nK=1\r\nK=2\n"))
	if len(got) != 1 || got["K"] != "2" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseDotenvLatin1(t *testing.T) {
	got := ParseDotenv([]byte("A=caf\xe9"))
	if got["A"] != "café" {
		t.Fatalf("got %q", got["A"])
	}
}

func TestLoadDotenvAndOverlay(t *testing.T) {
	dir := t.TempDir()
	if m, err := LoadDotenv(filepath.Join(dir, ".env")); err != nil || len(m) != 0 {
		t.Fatalf("missing file: %v %v", m, err)
	}
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("KEY=fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadDotenv(p)
	if err != nil {
		t.Fatal(err)
	}
	proc := lookupFrom(map[string]string{"KEY": "stale", "OTHER": "o"})
	lookup := Overlay(m, proc)
	if v, _ := lookup("KEY"); v != "fromfile" {
		t.Errorf("KEY = %q, .env must win", v)
	}
	if v, _ := lookup("OTHER"); v != "o" {
		t.Errorf("OTHER = %q", v)
	}
	if _, ok := lookup("NONE"); ok {
		t.Error("NONE should be unset")
	}
}
