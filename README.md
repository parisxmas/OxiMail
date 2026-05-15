# OxiMail

**OxiMail** is a production mail server in Go — SMTP (inbound +
submission), IMAP, a webmail HTTP API with an Angular frontend, and a
layered spam pipeline — backed entirely by **OxiDB**.

> Status: **early.** The store layer, the inbound SMTP (MX) server, the
> IMAP server, the submission server (port 587), the outbound delivery
> queue, TLS (STARTTLS + implicit TLS), and the webmail API + frontend
> are built — mail can be received, read, sent, and relayed over
> encrypted connections — each with tests. The spam pipeline's
> three stages — connection-time (rate limiting, DNS blocklists,
> greylisting), envelope (SPF/DKIM/DMARC), and content (Rspamd) — are
> built. The webmail API does login, read, send, flag changes, move, and
> delete — no search or attachment download yet. The `oximailctl` admin
> CLI provisions domains, accounts, and aliases and generates per-domain
> DKIM keys; outbound mail is DKIM-signed by the delivery queue, which
> also returns a bounce to the sender on permanent failure. Observability
> is wired up: structured logging via slog, Prometheus metrics, and
> liveness / readiness probes. The IMAP server still stubs COPY and
> mailbox DELETE / RENAME. See "Roadmap" below.

## Architecture

The protocol surfaces sit on one storage layer:

```
        :25   SMTP (MX)   ─┐
        :587  Submission  ─┤
        :465  SMTPS       ─┤
        :143  IMAP        ─┼─►  spam pipeline ──►  store ──►  OxiDB
        :993  IMAPS       ─┤                         ▲          ├─ collections   (canonical metadata)
        :8080 Webmail API ─┘                         │          ├─ blob store    (message bodies)
                            outbound queue ──────────┘          └─ OxiMem        (ephemeral state)
        :9090 /metrics, /healthz, /readyz  (observability)
```

STARTTLS is advertised on 25 / 587 / 143 when a certificate is
configured; 465 / 993 are implicit-TLS listeners, started only then.
The webmail API (`internal/webmail`) is an HTTP+JSON surface backed
directly by the store — not via IMAP — for browser and mobile clients;
it serves HTTPS when a certificate is configured. The webmail frontend
(`web/`) is an Angular 21 SPA over that API; once built, the webmail
server also serves it as static files.

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

**Spam — layered**, cheapest checks first, short-circuiting on the
first non-Accept verdict:

1. *connection-time* — rate limiting, DNS blocklists, greylisting.
   **Built.** State is in memory (disposable: losing it just
   re-greylists); a background sweeper bounds it.
2. *envelope* — SPF / DKIM / DMARC. **Built.** Rejects only on a DMARC
   `p=reject` failure (neither SPF nor DKIM passes, aligned with the
   From domain); a `p=quarantine`, `p=none`, missing record, or DNS
   error all accept. Fails open.
3. *content* — Rspamd over HTTP. **Built**, and active when
   `OXIMAIL_RSPAMD_URL` is set: the message is POSTed to Rspamd's
   `/checkv2`, and its action maps to accept / greylist / reject. It
   fails open — a Rspamd outage degrades filtering, it does not block
   mail.

`spam.Permissive()` builds a pipeline that accepts everything — for
tests, and for operators who filter elsewhere.

## Layout

```
cmd/oximail/         server entry point — config, wiring, graceful shutdown
cmd/oximailctl/      administration CLI — domains, accounts, aliases
internal/config/     configuration, loaded from the environment
internal/store/      the OxiDB-backed data layer
internal/smtp/       inbound SMTP (MX) + submission
internal/imap/       IMAP server
internal/webmail/    HTTP+JSON API for browser / mobile clients
internal/observability/  /metrics, /healthz, /readyz; slog setup
internal/spam/       the layered spam pipeline
internal/queue/      outbound delivery queue
web/                 the webmail frontend — an Angular 21 SPA
```

## Build & run

```sh
go build ./cmd/oximail
OXIMAIL_OXIDB_HOST=127.0.0.1 OXIMAIL_OXIDB_PORT=4444 ./oximail
```

Configuration is via `OXIMAIL_*` environment variables — see
`internal/config`. Every setting has a working default.

To enable TLS, point `OXIMAIL_TLS_CERT` and `OXIMAIL_TLS_KEY` at a PEM
certificate and key: STARTTLS is then advertised on 25 / 587 / 143, the
implicit-TLS listeners (465 / 993) are started, and cleartext AUTH /
LOGIN is refused. With no certificate set, the server runs without TLS
and allows cleartext auth — fine for local development, not for a real
deployment.

To serve the webmail frontend, build it and point the server at the
output:

```sh
cd web && npm install && npm run build      # -> web/dist/oximail-webmail/browser/
OXIMAIL_WEBMAIL_STATIC=web/dist/oximail-webmail/browser ./oximail
```

Without `OXIMAIL_WEBMAIL_STATIC` the webmail port serves the JSON API
only. See `web/README.md` for the frontend.

## Administration

`oximailctl` provisions domains, accounts, and aliases directly against
the store. It reads the same `OXIMAIL_OXIDB_*` environment variables as
the server.

```sh
go build ./cmd/oximailctl

oximailctl domain  add example.com
oximailctl domain  dkim example.com              # generate a DKIM key; prints the DNS record
echo 's3cret' | oximailctl account add -quota 1073741824 alice@example.com
oximailctl alias   add sales@example.com alice@example.com,bob@example.com
oximailctl account list
oximailctl account delete alice@example.com      # also removes its mail
```

`account add` reads the password from stdin. `domain dkim` generates a
signing key, stores it on the domain, and prints the public-key DNS TXT
record to publish — once published, outbound mail from the domain is
DKIM-signed by the delivery queue. Run `oximailctl help` for the full
command list.

## Observability

OxiMail exposes a dedicated HTTP port (`OXIMAIL_METRICS_ADDR`, default
`:9090`) for ops:

- `GET /metrics` — Prometheus exposition: `oximail_smtp_messages_total`
  by verdict, `oximail_queue_deliveries_total` by result,
  `oximail_queue_due_messages`, `oximail_logins_total` by protocol and
  result.
- `GET /healthz` — liveness; always 200 if the binary is up.
- `GET /readyz` — readiness; 200 when the store is reachable, 503
  otherwise.

Logging is structured via `log/slog`. `OXIMAIL_LOG_FORMAT=json` switches
the handler to JSON for log aggregators; `OXIMAIL_LOG_LEVEL` accepts
`debug` / `info` / `warn` / `error`. Legacy `log.Print` calls are
routed through slog automatically.

## Testing

`go test ./...` runs the fast unit tests — `internal/config` (TLS
config loading) and `internal/spam` (the connection-time stage, with an
injectable clock and DNS resolver, so no network or `oxidb-server` is
needed).

The integration tests boot a throwaway `oxidb-server` and exercise a
layer end to end:

- **store** — schema, entity CRUD, cascade delete, concurrent UID
  allocation.
- **smtp** — a real SMTP client delivering into a mailbox, recipient
  rejection, (submission) authenticated send splitting local delivery
  from queued relay, and STARTTLS / implicit-TLS submission.
- **imap** — a real IMAP client doing LOGIN / LIST / SELECT / FETCH /
  STORE / APPEND / EXPUNGE / SEARCH, over plaintext and over
  STARTTLS / IMAPS.
- **webmail** — a real HTTP client doing login, mailbox / message
  listing, fetching a parsed message, sending, flag changes, move and
  delete, and the auth / cross-account access rejections.
- **queue** — the worker delivering a queued message to a throwaway
  remote MX, and deferring one when the MX is unreachable.
- **oximailctl** — the admin CLI provisioning a domain, an account
  (then authenticating as it), and an alias, then deleting them.
- **observability** — `/healthz`, `/readyz` against a working and a
  closed store, and a counter increment reflected in `/metrics`.

They are gated behind a build tag:

```sh
go test -tags=integration ./...
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
   STORE, APPEND, EXPUNGE, SEARCH.~~ *Done.* Still to do: COPY, mailbox
   DELETE / RENAME, SASL AUTHENTICATE.
4. ~~Submission (port 587) + the outbound delivery queue — SMTP AUTH,
   local/remote recipient split, MX delivery with retry/backoff.~~
   *Done.*
5. ~~TLS — STARTTLS on 25 / 587 / 143, implicit TLS on 465 / 993,
   cleartext auth refused once a certificate is configured.~~ *Done.*
6. ~~Spam pipeline — connection-time (rate limiting, DNS blocklists,
   greylisting), envelope (SPF / DKIM / DMARC), and content (Rspamd)
   stages.~~ *Done.*
7. Webmail — *backend API done (login, mailbox / message listing,
   parsed message fetch, send, flag changes, move, delete) and an
   Angular 21 SPA frontend (`web/`) over it.* Still to do: search,
   attachment download, and HTML compose.
8. ~~Administration — `oximailctl` CLI for domains, accounts, and
   aliases, plus per-domain DKIM key generation.~~ *Done.* Still to do:
   a password-change command, and domain delete.
9. ~~Outbound DKIM signing — the delivery queue signs each message with
   the sender domain's key.~~ *Done.*
10. ~~Bounce messages — the queue returns an RFC 3464 delivery-status
    notification to the sender on a permanent failure or exhausted
    retries; never bounces a null-sender message.~~ *Done.*
11. ~~Observability — structured logging via `log/slog`, Prometheus
    metrics (`/metrics`), and liveness / readiness probes on a
    dedicated `:9090`.~~ *Done.*

Beyond the roadmap: IMAP COPY / mailbox DELETE / RENAME / SASL.
