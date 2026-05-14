module github.com/parisxmas/OxiMail

go 1.23

require (
	github.com/emersion/go-smtp v0.24.0
	github.com/parisxmas/OxiDB/go/oxidb v0.0.0
)

require (
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/parisxmas/OxiDB/go/oxiwire v0.0.0 // indirect
)

// OxiDB is the backing store — collections, blob store, and OxiMem. It
// is consumed from the sibling OxiDB checkout via a path replace.
replace github.com/parisxmas/OxiDB/go/oxidb => ../docdb/go/oxidb

replace github.com/parisxmas/OxiDB/go/oxiwire => ../docdb/go/oxiwire
