package common

import (
	"net"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalHosts_ContainsBaseline(t *testing.T) {
	hosts := LocalHosts()
	m := toSet(hosts)
	assert.Contains(t, m, "localhost", `LocalHosts() missing "localhost"`)
	assert.Contains(t, m, "127.0.0.1", `LocalHosts() missing "127.0.0.1"`)
}

func TestLocalHosts_ContainsHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skip("os.Hostname() unavailable")
	}
	hosts := LocalHosts()
	m := toSet(hosts)
	assert.Contains(t, m, hostname, "LocalHosts() missing hostname %q", hostname)
}

func TestLocalHosts_NoDuplicates(t *testing.T) {
	hosts := LocalHosts()
	seen := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		_, ok := seen[h]
		assert.False(t, ok, "duplicate entry: %q", h)
		seen[h] = struct{}{}
	}
}

func TestLocalHosts_Sorted(t *testing.T) {
	hosts := LocalHosts()
	assert.True(t, sort.StringsAreSorted(hosts), "LocalHosts() not sorted: %v", hosts)
}

func TestLocalHosts_ContainsNonLoopbackIP(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skip("net.Interfaces() unavailable")
	}
	hasNonLoopback := false
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ip := ipNet.IP
				if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
					hasNonLoopback = true
					break
				}
			}
		}
		if hasNonLoopback {
			break
		}
	}
	if !hasNonLoopback {
		t.Skip("no non-loopback network interfaces with IPs")
	}

	hosts := LocalHosts()
	for _, h := range hosts {
		ip := net.ParseIP(h)
		if ip != nil && !ip.IsLoopback() {
			return // found at least one
		}
	}
	assert.Fail(t, "LocalHosts() contains no non-loopback IP despite available interfaces")
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}
