package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome_Tilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":         home,
		"~/foo":     filepath.Join(home, "foo"),
		"~/foo/bar": filepath.Join(home, "foo/bar"),
		"/absolute": "/absolute",
		"relative":  "relative",
		"":          "",
	}
	for in, want := range cases {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q; want %q", in, got, want)
		}
	}
}
