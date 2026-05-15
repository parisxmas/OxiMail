module github.com/parisxmas/OxiMail

go 1.25.0

require (
	blitiri.com.ar/go/spf v1.5.1
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-msgauth v0.7.0
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.24.0
	github.com/parisxmas/OxiDB/go/oxidb v0.0.0
	github.com/prometheus/client_golang v1.23.2
	golang.org/x/crypto v0.51.0
	golang.org/x/net v0.53.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/parisxmas/OxiDB/go/oxiwire v0.0.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)

// OxiDB is the backing store — collections, blob store, and OxiMem. It
// is consumed from the sibling OxiDB checkout via a path replace.
replace github.com/parisxmas/OxiDB/go/oxidb => ../docdb/go/oxidb

replace github.com/parisxmas/OxiDB/go/oxiwire => ../docdb/go/oxiwire

// go-imap runs from a sibling fork with the CONDSTORE server-side
// patch from patches/go-imap/ applied on top of upstream
// v2.0.0-beta.8. Drop this replace once the patch lands upstream and
// a release is cut.
replace github.com/emersion/go-imap/v2 => ../go-imap
