# warren

A self-hosted, open-source alternative to ngrok. Run the server on your own VPS,
then you and your friends can expose local services through it, with no limits
other than your VPS's bandwidth.

- **TCP tunnels**: Minecraft Java, Terraria, SSH, databases, anything TCP
- **UDP tunnels**: Minecraft Bedrock, Factorio, other UDP games
- **Website tunnels**: `https://name.tunnel.example.com` → your local web app (websockets work)
- Separate token per friend, add or remove users without restarting
- Auto-reconnect that keeps the same public port/subdomain
- Optional TLS, optional PROXY protocol (so your Minecraft server sees real player IPs)
- One small static binary with no dependencies, for Linux, Windows, and macOS

```
 player ──► tunnel.example.com:25565 ──► warren server (VPS) ══► warren client ──► localhost:25565
```

The client makes an outgoing connection to the VPS, so whoever hosts the game
needs **no port forwarding on their router**.

## Guides

- **[Installing the server on your VPS](docs/INSTALL.md)**: on the VPS, run
  ```sh
  wget https://raw.githubusercontent.com/proshiv85-byte/warren/main/setup.sh
  sudo bash setup.sh
  ```
  and answer a few questions. Or follow the manual steps (DNS, firewall,
  systemd, TLS, managing friends, troubleshooting)
- **[Using warren](docs/USAGE.md)**: for you and your friends, covering
  Minecraft, websites, other games, and troubleshooting

- **[Minecraft mod](minecraft/README.md)**: share a singleplayer "Open to LAN"
  world with `/warren open` (Fabric 1.21.11; friends don't need the mod)
- **[Roadmap](docs/ROADMAP.md)**: planned improvements

## Quick look

```sh
# on the VPS
warren adduser alice          # prints a login command to send Alice

# on Alice's PC
warren login --server tunnel.example.com:7000 --token ... --tls
warren tcp 25565 --port 25565             # Minecraft Java
warren udp 19132 --port 19132             # Minecraft Bedrock
warren http 3000 --subdomain alice        # https://alice.tunnel.example.com
warren tcp 25565 -b                       # in the background (warren ps, warren stop)
warren update                             # update to the latest release
```

## Server config reference (`/etc/warren/server.json`)

| Key | Meaning |
|---|---|
| `listen` | Control port that clients connect to (default `:7000`) |
| `domain` | Your tunnel domain |
| `port_min` / `port_max` | Ports users may claim for TCP/UDP tunnels |
| `http_addr` | Address for website tunnels, e.g. `:80`. Leave empty to disable |
| `https_addr`, `https_cert`, `https_key` | HTTPS for website tunnels; the cert must cover `*.domain` |
| `public_http_port` | Port shown in website URLs when a reverse proxy sits in front |
| `tls_cert`, `tls_key` | Encrypt client↔server traffic, including tokens (recommended) |
| `users` | Managed by `adduser` / `deluser` |

Admin commands read `/etc/warren/server.json` by default. Override it with
`--config FILE` or `WARREN_CONFIG`.

## Notes and limitations

- UDP is carried over TCP between the client and server. That's fine for
  games, but on a lossy connection it adds more latency than native UDP would.
- Website tunnels pass traffic through untouched, so no `X-Forwarded-For`
  header is added.
- Running the same tunnel (same user + port/subdomain) twice: the newer one
  replaces the older one.

## Building

Requires Go 1.22+.

```sh
make build   # ./warren for this machine
make dist    # dist/ for linux, windows, mac (amd64 + arm64)
```

## License

MIT
