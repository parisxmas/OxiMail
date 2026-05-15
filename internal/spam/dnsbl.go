package spam

import (
	"fmt"
	"net"
	"strings"
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
	q := reverseIP(ip)
	if q == "" {
		return false // not an address we can query
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

// reverseIP turns an IPv4 or IPv6 address into the reversed form used
// for a DNSBL query name: "1.2.3.4" → "4.3.2.1"; an IPv6 address is
// nibble-reversed, 32 dot-separated hex digits. Returns "" for anything
// unparseable.
func reverseIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d", v4[3], v4[2], v4[1], v4[0])
	}
	v6 := parsed.To16()
	if v6 == nil {
		return ""
	}
	nibbles := make([]string, 0, 32)
	for i := len(v6) - 1; i >= 0; i-- {
		nibbles = append(nibbles, fmt.Sprintf("%x", v6[i]&0x0f))
		nibbles = append(nibbles, fmt.Sprintf("%x", v6[i]>>4))
	}
	return strings.Join(nibbles, ".")
}
