# go-imap CONDSTORE + QRESYNC patch (RFC 7162)

A draft patch for [emersion/go-imap](https://github.com/emersion/go-imap) v2
that adds server-side support for both CONDSTORE (RFC 7162 §3) and QRESYNC
(RFC 7162 §4), plus the small client-side pieces needed to actually drive
the new wire forms.

The protocol-level types and the client-side commands for CONDSTORE
(`imap.FetchOptions.ModSeq`, `imap.SelectData.HighestModSeq`, etc.) already
exist in v2; the patch fills in the imapserver glue. QRESYNC needed a few
new types in the `imap` package as well — `SelectOptions.QResync`,
`SelectData.Vanished`, and the `QResyncOptions` struct.

## Files changed

### Server-side (imapserver)

- `fetch.go` — parse `(CHANGEDSINCE N)` and `VANISHED` fetch
  modifiers; recognise the `MODSEQ` fetch data item; add
  `FetchResponseWriter.WriteModSeq(uint64)` so sessions can emit
  the `MODSEQ (n)` wire token (RFC 7162 §3.1.4, §3.2); add
  `FetchWriter.WriteVanished(uids)` so sessions can emit the
  `* VANISHED (EARLIER) <uids>` reply to a UID FETCH (CHANGEDSINCE
  N VANISHED) (RFC 7162 §3.2.10). VANISHED is validated at parse
  time: requires CHANGEDSINCE to have been present in the same
  modifier list, and at the command level requires UID FETCH.
- `store.go` — parse the `(UNCHANGEDSINCE N)` store modifier
  (RFC 7162 §3.1.3) and surface it on `StoreOptions.UnchangedSince`.
- `search.go` — parse the `MODSEQ` search criterion in both the bare
  and the extended forms (RFC 7162 §3.1.5); emit `MODSEQ N` in the
  ESEARCH response; auto-set `ReturnModSeq` when the criteria
  reference it.
- `select.go` — parse the `(CONDSTORE)` and `(QRESYNC ...)` SELECT
  modifiers (RFC 7162 §3.1.8, §3.2); emit `* OK [HIGHESTMODSEQ n]`
  when `SelectData.HighestModSeq != 0`; emit
  `* VANISHED (EARLIER) <uids>` when `SelectData.Vanished` is
  non-empty.
- `status.go` — recognise the `HIGHESTMODSEQ` STATUS data item and
  emit it in the response (RFC 7162 §3.1.2.2).
- `expunge.go` — add `ExpungeWriter.WriteVanished(uids)` for the
  post-EXPUNGE coalesced `* VANISHED <uids>` form a QRESYNC session
  uses in place of per-message `WriteExpunge`.
- `tracker.go` — `trackerUpdate` grows an `expungeUID` field;
  `QueueExpungeWithUID(seqNum, uid)` lets QRESYNC-aware servers
  attach the UID, while the existing UID-less `QueueExpunge` stays
  for backward compat. `SessionTracker.Poll` now routes expunge
  updates through `UpdateWriter.WriteExpungeUID`, which checks the
  connection's enabled-caps and emits `* VANISHED <uid>` to QRESYNC
  sessions (RFC 7162 §3.7) and the legacy `EXPUNGE seq` form to
  everyone else.
- `conn.go` — `UpdateWriter.WriteExpungeUID(seqNum, uid)` is the new
  routing helper described above.
- `enable.go` — `ENABLE CONDSTORE` and `ENABLE QRESYNC` both
  accepted; enabling QRESYNC implicitly also enables CONDSTORE per
  RFC 7162 §3.7.
- `capability.go` — `CapCondStore` and `CapQResync` added to the
  cross-rev backend-supported allowlist so an operator's
  `Options.Caps` map containing either is actually advertised
  (without this, the framework silently drops the value).

### Protocol-types (imap)

- `search.go` — `SearchOptions.ReturnModSeq` field.
- `select.go` — `SelectOptions.QResync *QResyncOptions`,
  `SelectData.Vanished UIDSet`, and the `QResyncOptions` struct
  carrying `UIDValidity`, `ModSeq`, `KnownUIDs`, plus the rare
  reconciliation-pair fields surfaced but optional.

### Client-side (imapclient)

- `enable.go` — `CapCondStore` and `CapQResync` added to the local
  allowlist so `c.Enable(...)` does not refuse them.
- `select.go` — emit `(QRESYNC ...)` when the caller populates
  `SelectOptions.QResync`.
- `fetch.go` — emit `VANISHED` in the FETCH modifier list when
  `FetchOptions.Vanished` is set; pair it with `CHANGEDSINCE`
  unconditionally (even when the value is zero) so the request
  passes the server-side validation we added in the same patch.
  `FetchCommand` gains `VanishedUIDs()` so callers can read the
  EARLIER UID set after `Close`/`Collect`.
- `expunge.go` + `client.go` — dispatch the `* VANISHED` response.
  The `(EARLIER)` variant attaches to a pending SELECT
  (`SelectData.Vanished`) OR to a pending FETCH
  (`FetchCommand.VanishedUIDs`); the plain form is surfaced via
  `ExpungeCommand` and via a new `ExpungeCommand.VanishedUIDs()`
  accessor so callers can read the UID set off the command.

## Scope

This patch is everything an MTA actually needs to advertise CONDSTORE
and QRESYNC to real clients. Out of scope, deliberately:

- The `MODIFIED` response code on STORE under UNCHANGEDSINCE is the
  session's job to emit — the patched framework just passes the
  StoreOptions through; the session decides which UIDs to refuse and
  returns `&imap.Error{Type: OK, Code: "MODIFIED <set>", ...}`.
- The `CLOSED` response code on the previous mailbox during a
  CONDSTORE-enabled SELECT — already emitted in upstream's
  pre-existing `Previous mailbox is now closed` path.

## Applying the patch

To upstream `emersion/go-imap` at tag `v2.0.0-beta.8`:

```sh
git apply patches/go-imap/condstore-qresync.patch
```

The patch applies cleanly and all existing upstream tests still pass —
verified with `go test ./...` against a freshly-patched checkout.

To use the patched library from another project before upstream
merges, point `replace` at the public fork on GitHub at the patch
commit (the fork's `v2` path satisfies Go's major-version constraint
on `/v2` modules):

```
# go.mod
replace github.com/emersion/go-imap/v2 => github.com/parisxmas/go-imap/v2 v2.0.0-beta.8.0.20260517135356-4b395c25f04d
```

This is what OxiMail's own `go.mod` does — the fork is at
[parisxmas/go-imap](https://github.com/parisxmas/go-imap), branch
`condstore-qresync`, awaiting upstream merge of
[emersion/go-imap#756](https://github.com/emersion/go-imap/pull/756).

## What a CONDSTORE / QRESYNC-aware OxiMail session looks like

OxiMail's `internal/imap` and `internal/store` already wire both
extensions end-to-end against this patch:

- `Mailbox.HighestModSeq` and `Message.ModSeq` are persisted; a
  `NextModSeq(mailboxID)` allocator is the atomic bump.
- `AppendMessage`, `CopyMessage`, `MoveMessage`, and `modifyFlags`
  stamp a fresh mod-seq on every state change.
- `selectedMailbox.selectData` reports `HighestModSeq`;
  `fetch` honours `ChangedSince` and emits `WriteModSeq`;
  `storeFlags` honours `UnchangedSince` with the MODIFIED response;
  `search` matches `criteria.ModSeq` and reports the max.
- `DeleteMessage` records `(uid, modseq)` in a new expunge-log
  collection; `ExpungedSince(mailboxID, modseq)` answers the QRESYNC
  resync question.
- The session reads `Conn.EnabledCaps()` to decide whether to emit
  `VANISHED` or `EXPUNGE`. SELECT with `opts.QResync` populates
  `SelectData.Vanished` from the expunge log.

Integration tests in `internal/imap/integration_test.go` cover both
extensions end-to-end.
