package main

import (
	"runtime/debug"
	"testing"
)

func TestModuleVersion(t *testing.T) {
	info := func(v string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: v}} }
	cases := []struct {
		name, injected string
		info           *debug.BuildInfo
		ok             bool
		want           string
	}{
		{"ldflags win over build info", "0.2.0", info("v0.1.0"), true, "0.2.0"},
		{"go install reports the module version", "dev", info("v0.1.0"), true, "0.1.0"},
		{"a local build stays dev", "dev", info("(devel)"), true, "dev"},
		{"no build info stays dev", "dev", nil, false, "dev"},
		{"empty module version stays dev", "dev", info(""), true, "dev"},
		{"a VCS-stamped local build stays dev", "dev", info("v0.0.0-20261025142111-77a556fa5b8d+dirty"), true, "dev"},
		{"go install @main stays dev", "dev", info("v0.1.1-0.20261025142111-77a556fa5b8d"), true, "dev"},
		{"a prerelease tag is reported", "dev", info("v0.2.0-rc.1"), true, "0.2.0-rc.1"},
	}
	for _, c := range cases {
		if got := moduleVersion(c.injected, c.info, c.ok); got != c.want {
			t.Errorf("%s: moduleVersion = %q, want %q", c.name, got, c.want)
		}
	}
}
