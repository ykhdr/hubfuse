package main

import "testing"

func TestParseSrc(t *testing.T) {
	cases := []struct{ in, user, host, path string }{
		{"alice@127.0.0.1:/data", "alice", "127.0.0.1", "/data"},
		{"bob@example.com:", "bob", "example.com", ""},
		{"no-at-sign", "", "", ""},
	}
	for _, c := range cases {
		u, h, p := parseSrc(c.in)
		if u != c.user || h != c.host || p != c.path {
			t.Errorf("parseSrc(%q) = (%q,%q,%q); want (%q,%q,%q)",
				c.in, u, h, p, c.user, c.host, c.path)
		}
	}
}

func TestOptValue(t *testing.T) {
	got := optValue([]string{"reconnect", "IdentityFile=/k", "Port=22"}, "IdentityFile")
	if got != "/k" {
		t.Errorf("optValue = %q; want /k", got)
	}
}
