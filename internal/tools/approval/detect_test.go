package approval

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/parity.json holds verdicts from upstream detect_hardline_command,
// detect_dangerous_command and _check_sudo_stdin_guard for the same inputs.
type parityCase struct {
	Command   string `json:"command"`
	Hardline  string `json:"hardline"`
	Dangerous string `json:"dangerous"`
	Sudo      bool   `json:"sudo"`
}

func newDetector(t *testing.T) *Detector {
	t.Helper()
	d, err := NewDetector("", "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUpstreamParity(t *testing.T) {
	b, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []parityCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	d := newDetector(t)
	for _, c := range cases {
		t.Run(c.Command, func(t *testing.T) {
			h, _ := d.Hardline(c.Command)
			g, _ := d.Dangerous(c.Command)
			if h != c.Hardline || g != c.Dangerous || d.SudoStdin(c.Command) != c.Sudo {
				t.Errorf("hardline %q dangerous %q sudo %v; upstream %q %q %v", h, g, d.SudoStdin(c.Command), c.Hardline, c.Dangerous, c.Sudo)
			}
		})
	}
}
