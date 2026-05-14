package spam

import (
	"fmt"
	"net"
)

// dnsblChecker looks the connecting IP up in DNS blocklists. A listed IP
// is rejected outright — the well-run lists carry low false-positive
// rates — but a lookup that simply errors is treated as not-listed, so a
// flaky resolver never starts rejecting all mail.
type dnsblChecker struct {
	zones  []string
	lookup func(host string) ([]string, error) // swapped out in tests
}

func newDNSBLChecker(zones []string) *dnsblChecker {
	return &dnsblChecker{zones: zones, lookup: net.LookupHost}
}

// listed reports whether ip appears in any configured blocklist zone.
func (d *dnsblChecker) listed(ip string) bool {
	q := reverseIPv4(ip)
	if q == "" {
		return false // not a dotted-quad IPv4 address we can query
	}
	for _, zone := range d.zones {
		addrs, err := d.lookup(q + "." + zone)
		// A blocklist signals "listed" by resolving the query name to an
		// address; NXDOMAIN (and any other error) means not listed.
		if err == nil && len(addrs) > 0 {
			return true
		}
	}
	return false
}

// reverseIPv4 turns "1.2.3.4" into "4.3.2.1" for a DNSBL query name. It
// returns "" for anything that is not an IPv4 address.
//
// TODO: IPv6 DNSBL queries (the nibble-reversed ip6.arpa-style form).
func reverseIPv4(ip string) string {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", v4[3], v4[2], v4[1], v4[0])
}
