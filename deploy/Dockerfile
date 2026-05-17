# syntax=docker/dockerfile:1.6
#
# OxiMail container image.
#
# Build context: the PARENT directory that contains the three sibling
# checkouts side by side. OxiMail's go.mod has `replace` directives
# pointing at ../docdb (OxiDB Go bindings) and ../go-imap (the
# CONDSTORE/QRESYNC fork), so the build needs all three trees in
# scope.
#
# Build:
#   cd $(dirname $(pwd))                  # parent of mailserver/
#   docker build -t oximail \
#     -f mailserver/deploy/Dockerfile .
#
# Run (env vars documented in mailserver/README.md):
#   docker run --rm \
#     -p 25:25 -p 80:80 -p 143:143 -p 465:465 -p 587:587 \
#     -p 993:993 -p 8080:8080 -p 9090:9090 \
#     -e OXIMAIL_HOSTNAME=mail.example.com \
#     -e OXIMAIL_OXIDB_HOST=oxidb \
#     -e OXIMAIL_ACME_HOSTS=mail.example.com \
#     -v oximail-acme:/var/lib/oximail/acme-cache \
#     oximail

# -----------------------------------------------------------------------
# 1. Angular SPA bundle (no replace-directive constraints — self-contained)
# -----------------------------------------------------------------------
FROM node:22-bookworm-slim AS node-builder
WORKDIR /web
COPY mailserver/web/package.json mailserver/web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm install --no-audit --no-fund
COPY mailserver/web/ ./
RUN npm run build

# -----------------------------------------------------------------------
# 2. Go binaries (oximail + oximailctl)
# -----------------------------------------------------------------------
FROM golang:1.25-bookworm AS go-builder
WORKDIR /src
# Pull in the three sibling trees so the replace directives in
# mailserver/go.mod resolve.
COPY mailserver/ ./mailserver/
COPY docdb/      ./docdb/
COPY go-imap/    ./go-imap/

WORKDIR /src/mailserver
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/oximail   ./cmd/oximail && \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/oximailctl ./cmd/oximailctl

# -----------------------------------------------------------------------
# 3. Runtime
# -----------------------------------------------------------------------
FROM debian:bookworm-slim AS runtime
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tini && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system --gid 999 oximail && \
    useradd  --system --uid 999 --gid 999 --home-dir /var/lib/oximail --create-home oximail && \
    install -d -o oximail -g oximail /var/lib/oximail/acme-cache

COPY --from=go-builder   /out/oximail       /usr/local/bin/oximail
COPY --from=go-builder   /out/oximailctl    /usr/local/bin/oximailctl
COPY --from=node-builder /web/dist/oximail-webmail/browser /var/lib/oximail/web

USER oximail
WORKDIR /var/lib/oximail
ENV OXIMAIL_WEBMAIL_STATIC=/var/lib/oximail/web \
    OXIMAIL_ACME_CACHE=/var/lib/oximail/acme-cache

EXPOSE 25 80 143 465 587 993 8080 9090
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/oximail"]
