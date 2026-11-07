# OxiMail

A mail server in Go — SMTP (inbound + submission), IMAP, a webmail
HTTP API with an Angular SPA, and a layered spam pipeline — backed
entirely by **OxiDB**.

> **Status:** this Go version is a prototype. OxiMail will be rewritten
> in Rill.

OxiMail speaks RFCs:

- SMTP (5321), Submission (6409), STARTTLS, SMTPS, MTA-STS (8461).
- IMAP4rev1 with **MOVE** (6851), **CONDSTORE** + **QRESYNC** (7162),
  **IDLE** with cross-connection EXPUNGE / FETCH FLAGS broadcast,
  UIDPLUS, ENABLE, UNSELECT, SASL PLAIN.
- DKIM signing on outbound, DMARC enforcement (p=reject → 5xx,
  p=quarantine → Junk), SPF on inbound, **SRS** alias forwarding,
  **ARC** sealing on forwards, RFC 3464 DSN bounces, RFC 3834
  vacation auto-responder, a **Sieve** filter at delivery (a subset
  of RFC 5228, see [Sieve](#sieve)).
- ACME / Let's Encrypt auto-renewal (HTTP-01) and SIGHUP-driven
  static-cert hot-reload as a fallback.
- HTTP webmail API with HttpOnly session cookies + double-submit
  CSRF for browser clients, bearer tokens for programmatic ones.
  Sessions live in memory, so a restart logs everyone out.

The IMAP server uses a [forked
go-imap/v2](./patches/go-imap/condstore-qresync.patch) with the
CONDSTORE + QRESYNC server-side surface upstream is missing; the
patch is ~940 lines, applies clean to `v2.0.0-beta.8`, and is in
`patches/go-imap/` ready to submit upstream.

## Architecture

```
        :25   SMTP (MX)   ─┐
        :587  Submission  ─┤
        :465  SMTPS       ─┤
        :143  IMAP        ─┼─►  spam pipeline ──►  store ──►  OxiDB
        :993  IMAPS       ─┤    + AV scanner         ▲          ├─ collections   (canonical metadata)
        :8080 Webmail     ─┘                         │          └─ blob store    (message bodies, refcounted)
                            outbound queue ──────────┘
        :9090 /metrics, /healthz, /readyz
        :80   ACME HTTP-01 challenge (when OXIMAIL_ACME_HOSTS is set)
```

**Storage** uses two OxiDB tiers:

- *Collections* — `domains`, `accounts`, `aliases`, `mailboxes`,
  `messages`, `outbound_queue`, `vacations`, `sieve_scripts`,
  `expunge_log`, `blob_refs`. IMAP flags are a field on the
  `messages` doc; flag changes are single-doc updates.
- *Blob store* — RFC 5322 bodies, **reference-counted** so IMAP COPY
  shares bodies across mailboxes instead of duplicating.

OxiMem is not used. Disposable state (greylisting tuples, per-IP
rate-limit counters, the vacation-reply suppressor, webmail sessions)
lives in process memory and is lost on restart.

**Spam pipeline**, cheapest checks first, short-circuiting:

1. Connection-time — per-IP rate limit, DNS blocklist (`zen.spamhaus.org`
   by default), greylisting.
2. Envelope — SPF / DKIM / DMARC. `p=reject` → 550, `p=quarantine` →
   `Junk` folder, anything else → INBOX.

There is no content-scoring stage.

**Antivirus** ([`internal/av`](./internal/av/)) runs in process. It is
a SHA-256 lookup, not a content scanner: each MIME part of inbound mail
and each attachment composed in the webmail is hashed and looked up in
a signature set made of a built-in EICAR entry, an optional operator
file, and hashes the updater downloads from the abuse.ch MalwareBazaar
and ThreatFox feeds. It catches only files whose exact hash is listed.
A hit is a 550 on inbound mail and a refused send in the webmail.

**Limits and sender checks.** Delivery refuses mail for an account over
its quota. Submission refuses a sender address that is not the
account's own or one of its aliases. The outbound queue retries
temporary failures for about 5 days before bouncing.

**Webmail hardening.** HTML mail is reduced to an allowlist on the
server and shown in a sandboxed iframe with no script and no
same-origin access. Remote images stay blocked until the reader clicks
"Show images" on that message. Every response carries a CSP,
`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and
`Referrer-Policy: no-referrer`. Attachments are always downloads, and
their type is `application/octet-stream` unless it is on a short safe
list. Request bodies are capped: 8 KiB for login, 256 KiB for JSON, and
enough for the 25 MiB attachment limit on compose.

**Auth surfaces** all share a [per-IP leaky-bucket](./internal/ratelimit/)
brute-force shield (default 10 failures / 60 s window). The webmail
API uses HttpOnly cookies + a double-submit CSRF token for browser
clients, and `Authorization: Bearer` for programmatic clients
(integration tests, CLIs); CSRF is required only for cookie auth.

## Layout

```
cmd/oximail/             server entry point — config, wiring, signals
cmd/oximailctl/          admin CLI — domains, accounts, aliases, dkim,
                                     vacation, sieve, backup, restore
internal/store/          OxiDB-backed data layer
internal/smtp/           inbound SMTP (MX) + submission, ARC sealing
internal/imap/           IMAP server (incl. CONDSTORE, QRESYNC, IDLE)
internal/webmail/        HTTP+JSON API + SPA static-file serving
internal/observability/  /metrics, /healthz, /readyz, slog setup
internal/spam/           layered spam pipeline
internal/queue/          outbound delivery queue, DKIM signing
internal/notifier/       cross-connection mailbox-change pub/sub
internal/sieve/          RFC 5228 subset interpreter (used at delivery)
internal/av/             SHA-256 attachment scanner + signature feed updater
internal/vacation/       RFC 3834 auto-responder + reply builder
internal/srs/            Sender Rewriting Scheme for alias forwarding
internal/mtasts/         MTA-STS policy lookup + cache
internal/arc/            ARC sealing (RFC 8617 sealing side)
internal/ratelimit/      leaky-bucket auth shield
internal/config/         OXIMAIL_* env-var loader, TLS / ACME wiring
internal/itest/          shared integration-test harness
internal/notifier/       in-process mailbox-change pub/sub
patches/go-imap/         CONDSTORE + QRESYNC patch for emersion/go-imap
deploy/                  Dockerfile + systemd unit + example envs
web/                     Angular 21 SPA over the webmail API
```

## Build & run

### From source

```sh
go build ./cmd/oximail ./cmd/oximailctl
(cd web && npm install && npm run build)
OXIMAIL_OXIDB_HOST=127.0.0.1 OXIMAIL_OXIDB_PORT=4444 \
OXIMAIL_HOSTNAME=mail.example.com \
OXIMAIL_TLS_CERT=/etc/oximail/tls.crt OXIMAIL_TLS_KEY=/etc/oximail/tls.key \
OXIMAIL_WEBMAIL_STATIC=web/dist/oximail-webmail/browser \
./oximail
```

### Docker (one host, OxiMail + OxiDB together)

The Dockerfile is standalone — `go.mod` fetches OxiDB and the
go-imap CONDSTORE/QRESYNC fork directly from GitHub, no sibling
checkouts needed:

```sh
git clone https://github.com/parisxmas/OxiMail
cd OxiMail

# Four env vars are required; everything else has a working default.
cat > deploy/.env <<EOF
OXIMAIL_HOSTNAME=mail.example.com
OXIMAIL_ACME_HOSTS=mail.example.com
OXIMAIL_ACME_EMAIL=ops@example.com
OXIMAIL_SRS_SECRET=$(openssl rand -hex 32)
EOF

docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f oximail
```

The compose file (`deploy/docker-compose.yml`) builds the OxiDB
image straight from its GitHub repo (`build.context:
https://github.com/parisxmas/OxiDB.git#master`), runs both
containers on a shared `oximail` bridge network so OxiMail reaches
OxiDB at the hostname `oxidb:4444`, exposes the eight OxiMail ports
on the host, and persists the ACME cert cache + the OxiDB data dir
as named volumes (`oximail-acme`, `oxidb-data`).

Building OxiMail by itself (without compose) is the same command:

```sh
docker build -t oximail -f deploy/Dockerfile .
```

### systemd

`deploy/oximail.service` is a stock unit file. Drop an env file at
`/etc/oximail/oximail.env` with the OXIMAIL_* vars and:

```sh
sudo install -m 0755 oximail /usr/local/bin/
sudo install -m 0644 deploy/oximail.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now oximail
```

## Configuration

Every setting has a working default. Set anything via `OXIMAIL_*`:

| Variable | Default | What it does |
|---|---|---|
| `OXIMAIL_HOSTNAME` | `localhost` | EHLO greeting, ARC d=, MTA-STS publish, mailbox HELO |
| `OXIMAIL_SMTP_ADDR` / `OXIMAIL_SMTPS_ADDR` | `:25` / `:465` | Inbound MX + SMTPS |
| `OXIMAIL_SUBMISSION_ADDR` | `:587` | Authenticated submission |
| `OXIMAIL_IMAP_ADDR` / `OXIMAIL_IMAPS_ADDR` | `:143` / `:993` | IMAP plaintext + IMAPS |
| `OXIMAIL_WEBMAIL_ADDR` | `:8080` | Webmail API + SPA |
| `OXIMAIL_WEBMAIL_STATIC` | `""` | SPA build dir; empty = API only |
| `OXIMAIL_METRICS_ADDR` | `:9090` | `/metrics`, `/healthz`, `/readyz` |
| `OXIMAIL_LOG_FORMAT` / `OXIMAIL_LOG_LEVEL` | `text` / `info` | slog handler + level |
| `OXIMAIL_TLS_CERT` / `OXIMAIL_TLS_KEY` | `""` | Static cert PEMs; SIGHUP reloads them |
| `OXIMAIL_ACME_HOSTS` | `""` | Comma-separated; ACME on when set (overrides static) |
| `OXIMAIL_ACME_CACHE` | `./acme-cache` | autocert disk cache |
| `OXIMAIL_ACME_EMAIL` | `""` | ACME account contact |
| `OXIMAIL_ACME_DIRECTORY_URL` | LE production | Override for LE staging |
| `OXIMAIL_ACME_CHALLENGE_ADDR` | `:80` | HTTP-01 listener |
| `OXIMAIL_OXIDB_HOST` / `OXIMAIL_OXIDB_PORT` | `127.0.0.1` / `4444` | OxiDB server |
| `OXIMAIL_AV_SIGDB` | `""` | Extra signature file, `<sha256>:<name>` per line |
| `OXIMAIL_AV_UPDATE_URL` | MalwareBazaar + ThreatFox full SHA-256 exports | Comma-separated feed URLs; set empty to disable updates |
| `OXIMAIL_AV_UPDATE_SOURCE` | `MalwareBazaar,ThreatFox` | Signature name for each feed, by position |
| `OXIMAIL_AV_UPDATE_INTERVAL` | `6h` | Time between feed refreshes |
| `OXIMAIL_AV_AUTO_SIGDB` | `/var/lib/oximail/av/auto.sigdb` | File the updater writes and the scanner loads |
| `OXIMAIL_SRS_SECRET` | `""` | Hex-encoded, ≥ 16 bytes raw, enables alias relay |
| `OXIMAIL_SRS_MAX_AGE` | `21 * 24h` | Bounce-address lifetime |
| `OXIMAIL_MTASTS_MODE` | `""` | `enforce` / `testing` / `none`; empty disables publish |
| `OXIMAIL_MTASTS_MX` | `OXIMAIL_HOSTNAME` | Comma-separated MX patterns in published policy |
| `OXIMAIL_MTASTS_MAX_AGE` | `86400s` | Policy lifetime |

## DNS records to publish

For `mail.example.com` running OxiMail with the defaults:

```
; A/AAAA — mail.example.com points at the OxiMail host.
mail.example.com.        IN A   203.0.113.10

; MX — example.com routes mail through mail.example.com.
example.com.             IN MX  10 mail.example.com.

; SPF — only mail.example.com is allowed to send for example.com.
example.com.             IN TXT "v=spf1 mx -all"

; DKIM — published by `oximailctl domain dkim example.com`,
; selector defaults to `oximail`.
oximail._domainkey.example.com. IN TXT "v=DKIM1; k=rsa; p=<base64 from oximailctl>"

; DMARC — start at p=quarantine so OxiMail files failures to Junk;
; tighten to p=reject once reports look clean.
_dmarc.example.com.      IN TXT "v=DMARC1; p=quarantine; rua=mailto:dmarc@example.com"

; MTA-STS — operator-published DNS record + an HTTPS-served policy
; file at https://mta-sts.example.com/.well-known/mta-sts.txt
; (OxiMail serves it when OXIMAIL_MTASTS_MODE is set).
_mta-sts.example.com.    IN TXT "v=STSv1; id=20260517T120000Z"
mta-sts.example.com.     IN A   203.0.113.10

; TLS-RPT — optional; OxiMail does not publish TLS reports itself
; yet, but receiving them works (route to a hosted account).
_smtp._tls.example.com.  IN TXT "v=TLSRPTv1; rua=mailto:tls-rpt@example.com"
```

## First message — walkthrough

```sh
# 1. provision a hosted domain
oximailctl domain add example.com

# 2. generate a DKIM key, publish the printed DNS TXT record
oximailctl domain dkim example.com

# 3. provision an account; the password is read from stdin
echo 'hunter2' | oximailctl account add -quota 1073741824 alice@example.com

# 4. (optional) set an out-of-office reply
oximailctl vacation set -subject 'Away' -body 'Back Monday.' alice@example.com

# 5. (optional) set a sieve filter
echo 'if header :contains "Subject" "report" { fileinto "Reports"; }' \
  | oximailctl sieve set alice@example.com

# 6. connect with any IMAP client to mail.example.com:993, log in
#    as alice@example.com / hunter2, or open the webmail at
#    https://mail.example.com:8080/

# 7. backup an account; refuses to overwrite an existing one on restore
oximailctl backup  alice@example.com /backup/alice.tar
oximailctl restore /backup/alice.tar
```

## Sieve

Scripts run at delivery, one per account. Only this subset of RFC 5228
is implemented:

- Control: `require`, `if` / `elsif` / `else`, `stop`.
- Actions: `keep`, `discard`, `fileinto` (with `:create`, RFC 5490).
- Tests: `header`, `address`, `exists`, `size`, `allof`, `anyof`,
  `not`, `true`, `false`.
- Match types `:is`, `:contains`, `:matches`; comparators
  `i;ascii-casemap` and `i;octet`; address parts `:all`, `:localpart`,
  `:domain`.
- `require` accepts `fileinto`, `mailbox`, `comparator-i;octet` and
  `comparator-i;ascii-casemap`.

Anything else, such as `redirect`, `reject`, `vacation`, `envelope`,
`imap4flags` or `fileinto :flags`, is refused with an error when the
script is set. A stored script that does not parse is skipped at
delivery, and the message gets default delivery.

## Observability

A dedicated port (`OXIMAIL_METRICS_ADDR`, default `:9090`):

- `GET /metrics` — Prometheus exposition: `oximail_smtp_messages_total`
  by verdict (accept, quarantine, reject, greylist),
  `oximail_queue_deliveries_total`, `oximail_queue_due_messages`,
  `oximail_logins_total` by protocol + result.
- `GET /healthz` — liveness; always 200 if the binary is up.
- `GET /readyz` — readiness; 200 when OxiDB is reachable, 503 otherwise.

Logging is `log/slog`. `OXIMAIL_LOG_FORMAT=json` switches the handler
to JSON for aggregators; `OXIMAIL_LOG_LEVEL` accepts `debug|info|warn|error`.

## Testing

Unit tests:

```sh
go test ./...
```

Integration tests boot a throwaway `oxidb-server` (set `OXIDB_BIN`
or place the binary at the sibling docdb checkout's
`target/{release,debug}/oxidb-server`):

```sh
go test -tags=integration ./...
```

## What's not yet implemented

The roadmap is long but a few items are explicit gaps:

- **The rest of Sieve.** See [Sieve](#sieve) for what exists.
- **JMAP** (RFC 8620/8621). A useful subset is months; the webmail
  HTTP API is JMAP-shaped but not JMAP-compliant.
- **ARC chain validation** (the verify side). OxiMail seals on
  forward; verifying inbound chains is a separate effort.
- **TLS-RPT aggregate reporting** (sender side). Operators can
  *receive* TLS-RPT reports today by routing `tls-rpt@…` to a hosted
  account; aggregate reporting from us as a sender is unimplemented.
- **CardDAV / CalDAV.** Out of scope.
- **POP3.** Strictly less capable than IMAP; intentionally not done.

## License

**AGPL-3.0-or-later.** See [LICENSE](./LICENSE) for the full text.

OxiMail is a network-facing service: if you modify the source and
run the modified version where users interact with it over a network
(an MX, an IMAP server, a webmail API, an admin API), §13 of the
AGPL requires you to offer those users the corresponding source of
the running version. Internal-only deployments are unaffected.
