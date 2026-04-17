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

// TestMain compiles the project's binaries into a tempdir and exposes them
// on PATH for testscript scenarios. Cleanup uses an explicit RemoveAll
// before os.Exit because deferred calls do not run after os.Exit, and each
// error path must replicate the cleanup. The discarded errors on RemoveAll
// satisfy golangci-lint's errcheck — the failures are not actionable here.
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

// TestCLI runs every .txtar scenario under testdata/.
//
// HOME is pinned to a per-script $WORK/home so scenarios can't read or
// write the developer's real ~/.hubfuse. Individual scripts may still
// override via `env HOME=...`.
//
// Binaries are reachable through PATH only — testscript does NOT resolve
// PATH binaries as bare commands, so scripts must call them with the
// `exec` prefix (e.g. `! exec hubfuse join`).
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
