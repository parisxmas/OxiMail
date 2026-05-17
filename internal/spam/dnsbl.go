package spam

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
)

// dnsblChecker looks the connecting IP up in DNS blocklists. A listed IP
// is rejected outright — the well-run lists carry low false-positive
// rates — but a lookup that simply errors is treated as not-listed, so a
// flaky resolver never starts rejecting all mail.
//
// Response handling follows RFC 5782 §2.1: a listing is signalled by a
// reply in the 127.0.0.0/8 range. The conventional codes are 127.0.0.2
// through 127.0.0.10 for actual listings (per-list semantics). Spamhaus
// (and a few others) reserve 127.255.255.x for diagnostic codes that
// mean "we refuse to answer this query" rather than "listed" —
// commonly because the query came in through a public open resolver
// (127.255.255.254) or because the operator has exceeded their quota
// (127.255.255.255). Treating those as listings would reject every
// inbound message; this checker maps them to "not listed" and logs the
// situation once per zone so the operator can fix the configuration.
type dnsblChecker struct {
	zones  []string
	lookup func(host string) ([]string, error) // swapped out in tests

	mu          sync.Mutex
	warnedZones map[string]bool
}

func newDNSBLChecker(zones []string) *dnsblChecker {
	return &dnsblChecker{
		zones:       zones,
		lookup:      net.LookupHost,
		warnedZones: make(map[string]bool),
	}
}

// listed reports whether ip appears in any configured blocklist zone.
func (d *dnsblChecker) listed(ip string) bool {
	if len(d.zones) == 0 {
		return false
	}
	q := reverseIP(ip)
	if q == "" {
		return false // not an address we can query
	}
	for _, zone := range d.zones {
		addrs, err := d.lookup(q + "." + zone)
		// NXDOMAIN (and any other error) means not listed.
		if err != nil || len(addrs) == 0 {
			continue
		}
		switch classifyDNSBLReply(addrs) {
		case dnsblListed:
			return true
		case dnsblRefused:
			// Spamhaus's "we refuse to answer queries via a public
			// resolver" code. Warn once so the operator notices,
			// then fail open.
			d.warnZoneOnce(zone, addrs)
		case dnsblUnknown:
			// Reply outside the documented 127.0.0.0/8 range. We
			// don't know what it means, so we don't reject on it.
			d.warnZoneOnce(zone, addrs)
		}
	}
	return false
}

// warnZoneOnce logs a one-shot warning about a zone whose reply we
// can't interpret as a listing. Repeat queries to the same zone stay
// quiet.
func (d *dnsblChecker) warnZoneOnce(zone string, addrs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.warnedZones[zone] {
		return
	}
	d.warnedZones[zone] = true
	log.Printf("spam: DNSBL %q returned %v — treating as 'not listed' "+
		"(127.255.255.x means the blocklist refused the query; "+
		"often because the lookup goes via a public DNS resolver — "+
		"set OXIMAIL_DNSBL_ZONES= to disable, or wire a private resolver "+
		"with a paid/registered query path)", zone, addrs)
}

// dnsblVerdict classifies a DNS-blocklist reply.
type dnsblVerdict int

const (
	dnsblListed  dnsblVerdict = iota // 127.0.0.x with x ∈ [1, 254]
	dnsblRefused                     // 127.255.255.x — Spamhaus error codes
	dnsblUnknown                     // anything else (not 127.0.0.0/8)
)

// classifyDNSBLReply returns the verdict for one DNSBL reply set.
// Multiple replies are possible when the IP is listed in several
// sub-lists of a composite zone (e.g. Spamhaus ZEN); we count it as
// "listed" if ANY reply is a real listing, otherwise we return the
// strictest non-listing classification.
func classifyDNSBLReply(addrs []string) dnsblVerdict {
	v := dnsblUnknown
	for _, a := range addrs {
		ip := net.ParseIP(a).To4()
		if ip == nil {
			continue
		}
		switch {
		case ip[0] == 127 && ip[1] == 255 && ip[2] == 255:
			// 127.255.255.x — diagnostic/error code (Spamhaus).
			if v == dnsblUnknown {
				v = dnsblRefused
			}
		case ip[0] == 127 && ip[3] >= 1 && ip[3] <= 254:
			// 127.x.y.z with z ∈ [1, 254] — RFC 5782 listing.
			return dnsblListed
		}
	}
	return v
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
