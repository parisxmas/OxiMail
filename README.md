# OxiMail

**OxiMail** is a production mail server in Go — SMTP (inbound +
submission), IMAP, and a layered spam pipeline — backed entirely by
**OxiDB**.

> Status: **early.** The structure, configuration, component wiring, the
> OxiDB-backed store layer, the inbound SMTP (MX) server, and the IMAP
> server are built, each with integration tests against a live
> `oxidb-server`. The IMAP server still stubs SEARCH, COPY, and mailbox
> DELETE / RENAME. The spam-pipeline, outbound-queue, and submission
> implementations are stubs. See "Roadmap" below.

## Architecture

Three protocol surfaces sit on one storage layer:

```
        :25  SMTP (MX)  ─┐
        :587 Submission ─┼─►  spam pipeline ──►  store ──►  OxiDB
        :143 IMAP       ─┘                         ▲          ├─ collections   (canonical metadata)
                          outbound queue ──────────┘          ├─ blob store    (message bodies)
                                                              └─ OxiMem        (ephemeral state)
```

**Storage — OxiDB, all three tiers:**

- **Collections** (canonical metadata): `domains`, `accounts`, `aliases`,
  `mailboxes`, `messages`, `outbound_queue`. Per-message IMAP flags are a
  field on the `messages` document, not a separate collection — the
  document is small (the body is a blob ref), so a flag change is a
  cheap single-document update.
- **Blob store** (message bodies): bodies live here, not in the
  `messages` documents — keeps documents small and stays clear of the
  16 MiB wire-frame cap (larger bodies go via the S3 API).
- **OxiMem** (ephemeral state): greylisting tuples, rate-limit counters,
  DNSBL caches.

Design constraints OxiDB imposes, and how the server handles them:

- *No atomic cross-document transactions* → IMAP `UIDNEXT` is allocated
  with `find_and_modify` (atomic single-doc read-modify-write); a crash
  may burn a UID, which IMAP tolerates.
- *No foreign keys / cascade* → referential cleanup (deleting an account
  removes its mailboxes, messages, and body blobs) is the store layer's
  job.
- *FTS is eventually consistent* → IMAP `SEARCH` over body text may lag
  delivery by a few seconds.

**Spam — layered**, cheapest checks first: connection-time (DNSBL,
greylisting, rate limits) → envelope (SPF/DKIM/DMARC) → content
(Rspamd) → feedback loop.

## Layout

```
cmd/oximail/         entry point — config, wiring, graceful shutdown
internal/config/     configuration, loaded from the environment
internal/store/      the OxiDB-backed data layer
internal/smtp/       inbound SMTP (MX) + submission
internal/imap/       IMAP server
internal/spam/       the layered spam pipeline
internal/queue/      outbound delivery queue
```

## Build & run

```sh
go build ./cmd/oximail
OXIMAIL_OXIDB_HOST=127.0.0.1 OXIMAIL_OXIDB_PORT=4444 ./oximail
```

Configuration is via `OXIMAIL_*` environment variables — see
`internal/config`. Every setting has a working default.

## Testing

The integration tests boot a throwaway `oxidb-server` and exercise a
layer end to end — the store (schema, entity CRUD, cascade delete,
concurrent UID allocation), the SMTP server (a real SMTP client
delivering into a mailbox, and recipient rejection), and the IMAP
server (a real IMAP client doing LOGIN / LIST / SELECT / FETCH / STORE /
APPEND / EXPUNGE). They are gated behind a build tag:

```sh
go test -tags=integration ./internal/...
```

The shared harness (`internal/itest`) finds the server binary at
`$OXIDB_BIN`, or at the sibling OxiDB checkout's
`target/{release,debug}/oxidb-server`; if neither exists the tests are
skipped.

## Roadmap

1. ~~Store layer — collection schema, entity types and operations, and
   an integration test against a live `oxidb-server`.~~ *Done.*
2. ~~Inbound SMTP (MX) on `emersion/go-smtp` — spam-pipeline call,
   recipient resolution, delivery into mailboxes.~~ *Done.*
3. ~~IMAP on `emersion/go-imap/v2` — LOGIN, LIST, SELECT, STATUS, FETCH,
   STORE, APPEND, EXPUNGE.~~ *Done.* Still to do: SEARCH, COPY, mailbox
   DELETE / RENAME, SASL AUTHENTICATE, and STARTTLS / IMAPS.
4. Spam pipeline — DNSBL / greylisting / rate limits, then SPF/DKIM/DMARC
   (`emersion/go-msgauth`), then Rspamd.
5. Outbound queue + submission (port 587).
