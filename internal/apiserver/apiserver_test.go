package apiserver

import "testing"

// TestAPILoopbackAddresses pins the binding rule the startup guard depends on:
// only addresses that cannot be reached from another host count as loopback.
func TestAPILoopbackAddresses(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"127.1.2.3:8787", true},
		{"localhost:8787", true},
		{"LocalHost:8787", true},
		{"[::1]:8787", true},
		{"0.0.0.0:8787", false},
		{"[::]:8787", false},
		{":8787", false},
		{"192.168.1.10:8787", false},
		{"example.com:8787", false},
		{"not-an-address", false},
	} {
		if got := Loopback(tc.addr); got != tc.want {
			t.Errorf("Loopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
