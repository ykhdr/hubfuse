package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
		assert.Equal(t, want, ExpandHome(in), "ExpandHome(%q)", in)
	}
}
