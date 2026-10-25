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
  for `.tunnel.example.com`, which reach every other user's site. Now that
  website tunnels go through a reverse proxy (0.6.0), it can strip
  `Domain=` from `Set-Cookie`.

## Small fixes

- Graceful shutdown on SIGTERM: tell clients "server restarting" so they
  reconnect right away and quietly.
- Lock `server.json` (`flock`) in `adduser`/`deluser`; two at once can lose
  an update.
- `takeBackgroundFlag` strips `-b` anywhere, even as another flag's value.
- Set `version` from the git tag via `-ldflags -X` instead of hardcoding it.

## Features

- Several tunnels from one config file (`warren start`, e.g. Java and
  Bedrock together), sharing one control connection.
- `warren service install` for clients: systemd user unit, launchd plist or
  Windows scheduled task, so tunnels survive a reboot.
- Server admin view: `warren tunnels` listing who has what open, connection
  counts, bytes in and out. Maybe a Prometheus `/metrics` endpoint.
- Per-user limits: reserved ports or subdomains, bandwidth caps, allowed
  protocols.
- Local request inspector like ngrok's `localhost:4040`, built on the
  request logs the server already sends (`http-log`).
- Native UDP transport (QUIC datagrams, or a UDP side channel authenticated
  with the stream ID) to avoid TCP head-of-line blocking.
- PROXY protocol v2, including for UDP (Geyser/Bedrock).

## Project

- Pooled connections (the `pool` capability) in the Minecraft mod.
- Packaging: Homebrew tap, Scoop or winget, and a Docker image for the
  server.
