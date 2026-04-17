package cli_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hubfuse-cli-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	binDir = dir

	repo, err := repoRoot()
	if err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		os.Exit(1)
	}

	for _, b := range []struct{ name, pkg string }{
		{"hubfuse", "./cmd/hubfuse"},
		{"hubfuse-hub", "./cmd/hubfuse-hub"},
	} {
		out := filepath.Join(binDir, b.name)
		cmd := exec.Command("go", "build", "-o", out, b.pkg)
		cmd.Dir = repo
		if combined, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(dir)
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, combined)
			os.Exit(1)
		}
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			env.Setenv("PATH", binDir+string(os.PathListSeparator)+env.Getenv("PATH"))
			return nil
		},
	})
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}
