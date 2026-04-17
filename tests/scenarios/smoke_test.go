package scenarios_test

import (
	"net"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestHubBootsAndStops(t *testing.T) {
	hub := helpers.StartHub(t)

	conn, err := net.DialTimeout("tcp", hub.Address, 1*time.Second)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	_ = conn.Close()

	// Cleanup runs via t.Cleanup; verify explicit stop is safe too.
	hub.Stop(t)
}
