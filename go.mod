module github.com/parisxmas/OxiMail

go 1.23

require github.com/parisxmas/OxiDB/go/oxidb v0.0.0

require github.com/parisxmas/OxiDB/go/oxiwire v0.0.0 // indirect

// OxiDB is the backing store — collections, blob store, and OxiMem. It
// is consumed from the sibling OxiDB checkout via a path replace.
replace github.com/parisxmas/OxiDB/go/oxidb => ../docdb/go/oxidb

replace github.com/parisxmas/OxiDB/go/oxiwire => ../docdb/go/oxiwire
