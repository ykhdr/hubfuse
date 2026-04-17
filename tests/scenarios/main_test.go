package scenarios_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

var (
	HubBinary       string
	AgentBinary     string
	StubSSHFSBinary string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hubfuse-scenarios-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}

	repo, err := repoRoot()
	if err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		os.Exit(1)
	}

	builds := []struct {
		varPtr *string
		out    string
		pkg    string
	}{
		{&HubBinary, "hubfuse-hub", "./cmd/hubfuse-hub"},
		{&AgentBinary, "hubfuse", "./cmd/hubfuse"},
		{&StubSSHFSBinary, "sshfs", "./tests/tools/stub-sshfs"},
	}
	for _, b := range builds {
		out := filepath.Join(dir, b.out)
		cmd := exec.Command("go", "build", "-o", out, b.pkg)
		cmd.Dir = repo
		if combined, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, combined)
			os.Exit(1)
		}
		*b.varPtr = out
	}

	helpers.HubBinaryPath = HubBinary
	helpers.AgentBinaryPath = AgentBinary
	helpers.StubSSHFSBinaryPath = StubSSHFSBinary

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// repoRoot returns the module root via `go env GOMOD`. This avoids the git
// dependency of `git rev-parse --show-toplevel` and works in module-zip
// extracts and sandboxed builds.
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
