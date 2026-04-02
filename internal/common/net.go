package common

import (
	"net"
	"os"
	"sort"
)

// LocalHosts returns a deduplicated, sorted list of hostnames and IP addresses
// for the local machine suitable for use as TLS certificate SANs. It always
// includes "localhost" and "127.0.0.1". It adds the machine's hostname and all
// unicast IPs from non-loopback, up network interfaces. Errors from
// net.Interfaces or os.Hostname are silently ignored — the function falls back
// to the baseline set.
func LocalHosts() []string {
	seen := map[string]struct{}{
		"localhost": {},
		"127.0.0.1": {},
	}

	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		seen[hostname] = struct{}{}
	}

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipNet.IP
				if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
				seen[ip.String()] = struct{}{}
			}
		}
	}

	hosts := make([]string, 0, len(seen))
	for h := range seen {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}
