# Using warren

warren lets you share a game server or website running on **your** computer
with anyone on the internet. You don't need to set up port forwarding on your
router, and there are no time limits or bandwidth caps.

You'll need two things from whoever runs the warren server:

1. **The warren program** for your system (see below)
2. **Your login command**, which looks like
   `warren login --server tunnel.example.com:7000 --token 3f9a…e1 --tls`

> Your token works like a password. Don't share it or post it in screenshots.

---

## 1. Install

Pick the file for your system:

| Your system | File |
|---|---|
| Windows | `warren-windows-amd64.exe` |
| Mac with Apple chip (M1/M2/M3/M4) | `warren-darwin-arm64` |
| Mac with Intel chip | `warren-darwin-amd64` |
| Linux (normal PC) | `warren-linux-amd64` |
| Linux on ARM (Raspberry Pi 64-bit…) | `warren-linux-arm64` |

### Windows

1. Create a folder, e.g. `C:\warren`, and put the file there.
2. Rename it to `warren.exe`.
3. Open the folder in File Explorer, click the address bar, type `powershell`
   and press Enter. A blue/black terminal window opens in that folder.
4. Test it:
   ```powershell
   .\warren.exe version
   ```

> On Windows, every command in this guide starts with `.\warren.exe` instead
> of `warren`. For example: `.\warren.exe tcp 25565`.
>
> If Windows SmartScreen says "Windows protected your PC", click **More info →
> Run anyway**. That warning appears for any program that isn't signed by a
> big company.

### Mac

Open **Terminal** and run (adjust the filename to the one you downloaded):

```sh
cd ~/Downloads
chmod +x warren-darwin-arm64
xattr -d com.apple.quarantine warren-darwin-arm64   # stops the "unidentified developer" block
sudo mv warren-darwin-arm64 /usr/local/bin/warren
warren version
```

### Linux

```sh
chmod +x warren-linux-amd64
sudo mv warren-linux-amd64 /usr/local/bin/warren
warren version
```

---

## 2. Log in (once)

Paste the login command you were given:

```sh
warren login --server tunnel.example.com:7000 --token YOUR_TOKEN --tls
```

This saves your settings, so you never have to type the token again.

---

## 3. Share something

The pattern is always:

```
warren <type> <local port> [options]
```

- **type**: `tcp`, `udp` or `http`. This depends on what you're sharing, see
  the examples below.
- **local port**: the port the server or app on your computer uses.

Leave the window open while you want the tunnel to be online. Press `Ctrl+C`
to stop. Or add `-b` to run it in the background and close the window (see
[Keeping it running in the background](#keeping-it-running-in-the-background-optional)).

### Minecraft Java Edition

1. Start your Minecraft server as usual (it runs on port `25565`).
2. In a second terminal window:
   ```sh
   warren tcp 25565 --port 25565
   ```
3. You'll see:
   ```
     warren is online

     tcp://tunnel.example.com:25565  ->  127.0.0.1:25565

     people connect to: tunnel.example.com:25565
   ```
4. Your friends add a server in Minecraft with the address
   **`tunnel.example.com:25565`**.

`--port 25565` asks for that public port. If someone else is already using it,
you'll get an error. Pick another one, like `--port 25566`, or leave `--port`
out entirely to get a random free port. Once you have a port, you keep it as
long as warren is running, even if your internet drops for a moment.

**Things to set in `server.properties`:**

- Keep `online-mode=true` so only real Minecraft accounts can join.
- Consider `white-list=true` and adding your friends with `/whitelist add NAME`.
  Your server is reachable by anyone on the internet who knows the address.

### Minecraft Bedrock Edition

Bedrock uses UDP on port `19132`:

```sh
warren udp 19132 --port 19132
```

Friends add a server with address `tunnel.example.com` and port `19132`.

### Java + Bedrock together (Geyser)

Run two tunnels, each in its own terminal window:

```sh
warren tcp 25565 --port 25565
warren udp 19132 --port 19132
```

### A website or web app

```sh
warren http 3000 --subdomain myproject
```

Your site is now at `https://myproject.tunnel.example.com` (or `http://`,
depending on the server). If you leave out `--subdomain`, you get a random
one. Use whatever port your app runs on, e.g. `8080`, `5173` (Vite), or `3000`
(Next.js / React).

> Some dev servers reject unknown hostnames. Vite needs
> `server.allowedHosts: ['.tunnel.example.com']` in `vite.config.js`. Others
> have a similar "allowed hosts" setting.

To keep a site private:

```sh
warren http 3000 --auth me:a-long-password     # visitors must log in
warren http 3000 --allow 203.0.113.7           # only this IP gets in
warren http 3000 --allow 203.0.113.0/24,198.51.100.4
```

The password protects the tunnel, so your app never sees it. If the server
runs an old warren that can't enforce `--auth` or `--allow`, your warren
refuses to start rather than leave the site open.

Your app gets the visitor's IP in the `X-Forwarded-For` header, and
`X-Forwarded-Proto` says whether they used `https`.

### Other games and programs

Anything that uses TCP or UDP works. You just need to know its port and
protocol (search "<game> server port"). Some examples:

| What | Command |
|---|---|
| Terraria | `warren tcp 7777 --port 7777` |
| Factorio | `warren udp 34197 --port 34197` |
| SSH into your PC | `warren tcp 22` |
| A server on another device in your home | `warren tcp 192.168.1.50:25565 --port 25565` |

**Tip:** use the same public port as the local port (`--port`) whenever you
can. Some games tell players which port to use, and that only works if both
match.

If a game needs **several ports**, run one warren command per port, each in
its own window.

---

## Seeing real player IPs (optional, Minecraft)

By default, your Minecraft server thinks every player comes from `127.0.0.1`.
That's fine for playing, but IP bans and some plugins won't work. To pass the
real IP through:

1. Turn on PROXY protocol in your server software:
   - **Paper**: in `config/paper-global.yml`, under `proxies:`, set
     `proxy-protocol: true`
   - **Velocity**: in `velocity.toml`, set `haproxy-protocol = true`
   - **BungeeCord/Waterfall**: in `config.yml`, under your listener, set
     `proxy_protocol: true`
2. Restart the Minecraft server.
3. Start warren with `--proxy-protocol`:
   ```sh
   warren tcp 25565 --port 25565 --proxy-protocol
   ```

> ⚠️ Use both settings together or neither. If only one side has it enabled,
> nobody can join.

Vanilla Minecraft, Spigot, and Bedrock don't support this.

---

## What you'll see in the window

```
2026/10/05 20:14:03 connection from 81.2.69.160:51234
2026/10/05 20:31:40 disconnected (EOF), reconnecting in 1s
2026/10/05 20:31:41 reconnected: tcp://tunnel.example.com:25565
```

- **connection from**: someone joined, with their IP address.

Website tunnels show each request instead:

```
2026/10/05 20:14:03 GET     200     12ms  81.2.69.160     /index.html
2026/10/05 20:14:03 GET     404      3ms  81.2.69.160     /favicon.ico
```

That's the method, the status your app answered, how long it took, the
visitor's IP and the path.
- **disconnected / reconnected**: your internet or the server blipped. warren
  reconnects by itself and keeps the same address, so you don't need to do
  anything.

---

## Keeping it running in the background (optional)

Add `-b` to any tunnel command. warren waits until the tunnel is online,
prints the address, and then runs on its own, so you can close the window:

```sh
warren tcp 25565 --port 25565 -b
```

```
  warren is online

  tcp://tunnel.example.com:25565  ->  127.0.0.1:25565

  people connect to: tunnel.example.com:25565

  running in the background (pid 4182)
  log:   /home/you/.config/warren/background/20261010-110405-tcp-25565.log
  stop:  warren stop 4182    list: warren ps
```

```sh
warren ps            # list background tunnels and their addresses
warren stop 4182     # stop one (use the PID from warren ps)
warren stop all      # stop all of them
```

If something is wrong (bad token, port taken…), `-b` shows the error right
away instead of hiding it in the background. It works the same on Windows,
Mac and Linux. A background tunnel stops when you shut down or log out.

**To start it automatically at boot on Linux**, use a systemd user service
instead. Create `~/.config/systemd/user/warren-mc.service`:

```ini
[Unit]
Description=warren tunnel for Minecraft

[Service]
ExecStart=/usr/local/bin/warren tcp 25565 --port 25565
Restart=always

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now warren-mc
loginctl enable-linger $USER     # keep running when you're logged out
journalctl --user -u warren-mc -f   # view the log
```

---

## Updating

```sh
warren update            # download and install the latest version
warren update --check    # only check whether there's a new one
```

On Linux and Mac, if warren is in `/usr/local/bin`, use `sudo warren update`.
On Windows, if warren is in a protected folder like `C:\Program Files`, run
PowerShell as administrator. Tunnels started with `-b` keep running the old
version until you restart them (`warren stop`, then start them again).

warren versions before 0.3.0 don't have this command yet. Download the new
version once from the releases page, and use `warren update` from then on.

---

## Troubleshooting

| Message | What to do |
|---|---|
| `no server/token configured` | Run the `warren login ...` command first. |
| `invalid token` | Check you copied the whole token. Ask the server owner if your access was removed. |
| `EOF (if the server uses TLS, add --tls)` | Run the login command again with `--tls` at the end. |
| `the server's certificate doesn't match --fingerprint` | The server's certificate changed. Ask the server owner for the new login command (or the output of `warren fingerprint`). If they didn't change anything, someone may be intercepting your connection. |
| `flag provided but not defined: -fingerprint` | Your warren is too old for this login command. Run `warren update`. |
| `connection timed out` / `connection refused` | The warren server is down or unreachable. Ask the server owner. |
| `tcp/25565 is already in use by someone else` | Another person is using that public port. Pick another `--port`. |
| `port N is outside the allowed range` | The server owner limited which ports can be used. Try one in the range shown. |
| `nothing is listening on 127.0.0.1:25565` | warren works, but your game server isn't running, or it uses a different port. Start it, or check the port number. |
| `replaced by a newer connection` | You started the same tunnel twice. The newer one took over. Close the extra window. |
| Friends get "Connection refused" in Minecraft | Check that your Minecraft server is running and that warren shows `online`. |
| Website shows `tunnel "x" is not online` | Your warren window for that subdomain isn't running. |
| Website shows `local service ... is not running` | Your web app isn't running, or it's on a different port. |
| `this server is too old to protect tunnels with --auth` | Ask the server owner to run `warren update`. Until then, leave out `--auth`/`--allow`. |
| Players see "Invalid IP / proxy" errors or can't join after enabling PROXY protocol | You enabled it on only one side. See [Seeing real player IPs](#seeing-real-player-ips-optional-minecraft). |

---

## Quick reference

```sh
warren login --server HOST:7000 --token TOKEN [--tls]   # once
warren tcp  PORT [--port PUBLIC_PORT] [--proxy-protocol]
warren udp  PORT [--port PUBLIC_PORT]
warren http PORT [--subdomain NAME] [--auth USER:PASS] [--allow IPS]
warren tcp  PORT -b                                     # run in the background
warren ps                                               # list background tunnels
warren stop PID|all                                     # stop them
warren update                                           # update to the latest version
warren tcp -h                                           # all options
```
