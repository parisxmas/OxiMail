# syntax=docker/dockerfile:1.6
#
# OxiMail container image.
#
# Build context: the OxiMail repo root (no sibling checkouts needed
# now that go.mod fetches OxiDB and the go-imap CONDSTORE/QRESYNC
# fork directly from GitHub).
#
# Build:
#   docker build -t oximail -f deploy/Dockerfile .
#
# Run alongside OxiDB via deploy/docker-compose.yml:
#   docker compose -f deploy/docker-compose.yml up -d

# -----------------------------------------------------------------------
# 1. Angular SPA bundle
# -----------------------------------------------------------------------
FROM node:22-bookworm-slim AS node-builder
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build

# -----------------------------------------------------------------------
# 2. Go binaries (oximail + oximailctl)
# -----------------------------------------------------------------------
FROM golang:1.25-bookworm AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
# Warm the module cache before copying source so a code-only change
# does not re-download the world.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
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
    install -d -o oximail -g oximail /var/lib/oximail/acme-cache && \
    install -d -o oximail -g oximail /var/lib/oximail/av

COPY --from=go-builder   /out/oximail       /usr/local/bin/oximail
COPY --from=go-builder   /out/oximailctl    /usr/local/bin/oximailctl
COPY --from=node-builder /web/dist/oximail-webmail/browser /var/lib/oximail/web

USER oximail
WORKDIR /var/lib/oximail
ENV OXIMAIL_WEBMAIL_STATIC=/var/lib/oximail/web \
    OXIMAIL_ACME_CACHE=/var/lib/oximail/acme-cache

EXPOSE 25 80 143 465 587 993 8080 9090
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/oximail"]
