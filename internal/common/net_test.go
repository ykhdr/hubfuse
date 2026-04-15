package common

import (
	"net"
	"os"
	"sort"
	"testing"
)

func TestLocalHosts_ContainsBaseline(t *testing.T) {
	hosts := LocalHosts()
	m := toSet(hosts)
	if _, ok := m["localhost"]; !ok {
		t.Error("LocalHosts() missing \"localhost\"")
	}
	if _, ok := m["127.0.0.1"]; !ok {
		t.Error("LocalHosts() missing \"127.0.0.1\"")
	}
}

func TestLocalHosts_ContainsHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skip("os.Hostname() unavailable")
	}
	hosts := LocalHosts()
	m := toSet(hosts)
	if _, ok := m[hostname]; !ok {
		t.Errorf("LocalHosts() missing hostname %q", hostname)
	}
}

func TestLocalHosts_NoDuplicates(t *testing.T) {
	hosts := LocalHosts()
	seen := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if _, ok := seen[h]; ok {
			t.Errorf("duplicate entry: %q", h)
		}
		seen[h] = struct{}{}
	}
}

func TestLocalHosts_Sorted(t *testing.T) {
	hosts := LocalHosts()
	if !sort.StringsAreSorted(hosts) {
		t.Errorf("LocalHosts() not sorted: %v", hosts)
	}
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
	t.Error("LocalHosts() contains no non-loopback IP despite available interfaces")
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}
