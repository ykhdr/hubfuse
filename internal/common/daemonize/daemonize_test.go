package daemonize

import (
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
