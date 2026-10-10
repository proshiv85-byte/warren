# Roadmap

Things worth doing that aren't urgent. Roughly in priority order within each
section.

## Hardening

- **Sign releases** (GitHub artifact attestations, cosign or minisign) and
  verify the signature in `warren update` and `setup.sh`. `SHA256SUMS` from
  the same release catches corruption but not a compromised release.
- **systemd hardening** in `warren.service`: `NoNewPrivileges=yes`,
  `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`,
  `ReadOnlyPaths=/etc/warren`, `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`.
- **Narrower default port range.** The 1024–65535 default overlaps the
  kernel's ephemeral range (32768+), so random binds sometimes collide and
  the whole range ends up open in the firewall. `setup.sh` writes explicit
  values, so this only affects hand-written configs.
- **Cookie scope on the shared domain.** Any website tunnel can set cookies
  for `.tunnel.example.com`, which reach every other user's site. Document
  it, or strip `Domain=` from `Set-Cookie` once website tunnels go through a
  real reverse proxy (below).

## Small fixes

- Graceful shutdown on SIGTERM: tell clients "server restarting" so they
  reconnect right away and quietly.
- Lock `server.json` (`flock`) in `adduser`/`deluser`; two at once can lose
  an update.
- `takeBackgroundFlag` strips `-b` anywhere, even as another flag's value.
- Set `version` from the git tag via `-ldflags -X` instead of hardcoding it.

## Architecture

- **Multiplex streams over one connection** (yamux/smux), or keep a small
  pool of idle data connections (like frp's `pool_count`) to stay
  dependency-free. Today every visitor waits for a fresh TCP+TLS handshake
  from the client to the server.
- **Website tunnels through `httputil.ReverseProxy`** with a transport that
  dials via `openStream`. That unlocks `X-Forwarded-For`/`-Proto`, routing
  per request, `--auth user:pass` and IP allowlists, an HTTP→HTTPS redirect,
  HTTP/2 to visitors, and request logs. WebSockets keep working.
- **Automatic certificates** with `autocert`, allowing only subdomains that
  have a live claim. Mind Let's Encrypt's limit of about 50 certificates per
  registered domain per week.
- **Protocol capabilities.** Clients could send a list of features and the
  server accept a range of versions, so a protocol change doesn't force
  everyone to update at once.

## Features

- Several tunnels from one config file (`warren start`, e.g. Java and
  Bedrock together) over a single connection.
- `warren service install` for clients: systemd user unit, launchd plist or
  Windows scheduled task, so tunnels survive a reboot.
- Server admin view: `warren tunnels` listing who has what open, connection
  counts, bytes in and out. Maybe a Prometheus `/metrics` endpoint.
- Per-user limits: reserved ports or subdomains, bandwidth caps, allowed
  protocols.
- Local request inspector like ngrok's `localhost:4040` (needs the reverse
  proxy above).
- Native UDP transport (QUIC datagrams, or a UDP side channel authenticated
  with the stream ID) to avoid TCP head-of-line blocking.
- PROXY protocol v2, including for UDP (Geyser/Bedrock).

## Project

- Release workflow: build `make dist` on a tag, generate `SHA256SUMS`, sign,
  publish, and attach the mod jar.
- Packaging: Homebrew tap, Scoop or winget, and a Docker image for the
  server.
