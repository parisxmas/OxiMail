# go-imap CONDSTORE patch (RFC 7162)

A draft patch for [emersion/go-imap](https://github.com/emersion/go-imap) v2
that adds server-side support for the CONDSTORE extension. The protocol-level
types (`imap.FetchOptions.ModSeq`, `imap.FetchOptions.ChangedSince`,
`imap.StoreOptions.UnchangedSince`, `imap.SelectOptions.CondStore`,
`imap.SelectData.HighestModSeq`, `imap.StatusOptions.HighestModSeq`,
`imap.SearchCriteria.ModSeq`, `imap.SearchData.ModSeq`) and the corresponding
client commands already exist in v2; only the `imapserver` package needed
extending so a server session can actually parse the CONDSTORE wire forms and
emit MODSEQ values back to the client.

## Files changed

- `search.go` — add `SearchOptions.ReturnModSeq` so a server can declare it
  must compute the highest mod-sequence over a matched set (RFC 7162 §3.1.5).
- `imapserver/fetch.go` — parse `(CHANGEDSINCE N)` fetch modifier;
  recognise the `MODSEQ` fetch data item; add
  `FetchResponseWriter.WriteModSeq(uint64)` so sessions can emit the
  `MODSEQ (n)` wire token (RFC 7162 §3.1.4, §3.2).
- `imapserver/store.go` — parse the `(UNCHANGEDSINCE N)` store modifier
  (RFC 7162 §3.1.3) and surface it on `StoreOptions.UnchangedSince`.
- `imapserver/search.go` — parse `MODSEQ` search criterion in both the
  bare `MODSEQ n` and the extended `MODSEQ "name" attrib n` forms
  (RFC 7162 §3.1.5); emit `MODSEQ n` in the ESEARCH response when set.
- `imapserver/select.go` — parse the `(CONDSTORE)` select modifier
  (RFC 7162 §3.1.8); emit the `* OK [HIGHESTMODSEQ n]` response when
  `SelectData.HighestModSeq != 0` (RFC 7162 §3.1.2.1).
- `imapserver/status.go` — recognise the `HIGHESTMODSEQ` STATUS data
  item and emit it in the response (RFC 7162 §3.1.2.2).

## Scope (and what's deliberately out)

This patch covers **CONDSTORE only**. The companion QRESYNC extension
(RFC 7162 §4) needs additional surface that this patch does not touch:

- A `VANISHED` response writer on `ExpungeWriter` (`WriteVanished(uids)`).
- `SelectData.UIDValidity` semantics for `(QRESYNC ...)` resynchronisation
  parameters.
- Parsing the `(QRESYNC uidvalidity modseq [known-uids] [...])` select
  parameter list.

The patch parses the `(CONDSTORE)` select parameter explicitly so that a
client mistakenly passing `(QRESYNC ...)` does not silently fall through —
it gets a `BAD` response naming the unknown modifier.

## Applying the patch

To `emersion/go-imap` upstream, at tag `v2.0.0-beta.8`:

```sh
git apply patches/go-imap/condstore.patch
```

To use the patched library from this repository before upstream merges,
clone `emersion/go-imap`, apply the patch, and add a `replace` directive
to OxiMail's `go.mod`:

```
replace github.com/emersion/go-imap/v2 => ../go-imap
```

After upstream merges, drop the `replace` directive and bump to the new
released version.

## What a CONDSTORE-aware OxiMail session looks like after this lands

1. Advertise the capability: add `imap.CapCondStore: {}` to the
   `Caps` set passed to `imapserver.New` in `internal/imap/imap.go`.
2. Track per-message mod-sequences in the store: a `mod_seq` field on
   `store.Message`, allocated by a new `store.NextModSeq(mailboxID)`
   counter bumped on every flag change and on every append.
3. Track `highest_modseq` on the mailbox, returned via
   `SelectData.HighestModSeq` and the `STATUS HIGHESTMODSEQ` reply.
4. In `selectedMailbox.fetch`: call `w.WriteModSeq(msg.ModSeq)` whenever
   `options.ModSeq` is true; filter the snapshot by
   `options.ChangedSince` when non-zero.
5. In `selectedMailbox.storeFlags`: when `options.UnchangedSince != 0`,
   refuse the update for any message whose current mod-seq exceeds it
   (a MODIFIED response is returned in the FETCH stream — that's the
   one piece this patch does NOT yet enable, since `MODIFIED` requires
   a small additional encoder method; see the follow-up note below).
6. In `selectedMailbox.search`: when `criteria.ModSeq != nil`, filter the
   snapshot by `msg.ModSeq >= criteria.ModSeq.ModSeq` and set
   `data.ModSeq` to the highest mod-seq across the result before
   returning.

## Follow-up: MODIFIED response code

RFC 7162 §3.1.3 says that when a CONDSTORE `STORE (UNCHANGEDSINCE n)`
hits messages whose mod-sequence is greater than n, the server replies
with `OK [MODIFIED <set>] STORE completed`. That's just a tagged-OK
response code carrying a sequence set — already representable through
`imap.StatusResponse{Code: ...}` if we accept a string form. The
patch does not enforce the unchanged-since check in the server (that's
the session's job), but documenting the intended response format here
so a session implementer knows what to emit.
