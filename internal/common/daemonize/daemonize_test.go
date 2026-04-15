package daemonize

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestIsChild_EnvUnset(t *testing.T) {
	t.Setenv(EnvDaemonized, "")
	if IsChild() {
		t.Fatal("IsChild() = true with env unset; want false")
	}
}

func TestIsChild_EnvSet(t *testing.T) {
	t.Setenv(EnvDaemonized, "1")
	if !IsChild() {
		t.Fatal("IsChild() = false with env set; want true")
	}
}

func TestIsChild_EnvZero(t *testing.T) {
	t.Setenv(EnvDaemonized, "0")
	if IsChild() {
		t.Fatal("IsChild() = true with env=0; want false (only \"1\" means child)")
	}
}

func TestWritePIDFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pidfile contents %q: %v", data, err)
	}
	if got != os.Getpid() {
		t.Fatalf("pidfile contents = %d; want %d", got, os.Getpid())
	}
}

func TestWritePIDFile_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	if err := os.WriteFile(path, []byte("99999999\nleftover-junk\n"), 0o644); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if strings.Contains(string(data), "leftover-junk") {
		t.Fatalf("pidfile still has old contents: %q", data)
	}
}
