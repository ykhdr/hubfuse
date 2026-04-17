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
		_ = os.RemoveAll(dir)
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
			_ = os.RemoveAll(dir)
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, combined)
			os.Exit(1)
		}
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			home := filepath.Join(env.WorkDir, "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				return err
			}
			env.Setenv("HOME", home)
			env.Setenv("PATH", binDir+string(os.PathListSeparator)+env.Getenv("PATH"))
			return nil
		},
	})
}

// repoRoot returns the directory containing go.mod by asking the Go toolchain.
// This avoids depending on `git` being installed and works in non-git contexts
// like module zip extracts or source snapshots.
func repoRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not in a Go module")
	}
	return filepath.Dir(gomod), nil
}
