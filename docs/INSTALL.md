# Installing the warren server on your VPS

This guide takes you from a fresh VPS to a working tunnel server that you and
your friends can use. It takes about 15 minutes.

## Quick setup (recommended)

`setup.sh` does everything in this guide for you and asks a few questions on
the way, including whether you have a domain. Without a domain, warren uses
your VPS's IP address and a self-signed certificate. Everything works except
website tunnels. Login commands for a self-signed server include
`--fingerprint sha256:…`, which lets clients check they're talking to your
server even though no certificate authority vouches for it. Print it any time
with `sudo -u warren warren fingerprint`.

**On the VPS** (SSH in first), download the script and run it:

```sh
wget https://raw.githubusercontent.com/proshiv85-byte/warren/main/setup.sh
sudo bash setup.sh
```

It downloads warren from the latest release (checking its checksum), installs
anything else it needs, and walks you through the rest. Nothing else to copy.

**Or from your own computer** (needs Go and this repo), to install your own
build. One command builds warren, copies it to the VPS and runs the setup there:

```sh
./setup.sh ubuntu@203.0.113.10          # add -p 2222 if SSH isn't on port 22
```

(If a `warren-linux-*` binary is next to `setup.sh` on the VPS, it uses that
instead of downloading.)

At the end it prints your login command. Run `setup.sh` again any time to
update warren, change settings or add a user (tokens are kept), or with
`--uninstall` to remove it.

If you set up DNS (Step 1) before running it, Let's Encrypt works on the first
try. The rest of this guide explains what the script does, if you'd rather do
it by hand or need to fix something.

---

## Manual setup

**What you need**

- A VPS with a public IP, running Ubuntu or Debian. Other Linux distros work
  too, but some package commands will differ.
- SSH access to it as a user with `sudo` (e.g. `ubuntu`) or as `root`
- A domain name where you can edit DNS records
- This repo on your own computer, with Go installed (only to build the binary)

**Placeholders used in this guide.** Replace them everywhere you see them:

| Placeholder          | Meaning                                   | Example        |
|----------------------|-------------------------------------------|----------------|
| `tunnel.example.com` | the (sub)domain your tunnels will live on | `play.mydomain.net` |
| `203.0.113.10`       | your VPS's public IP                      | |

Using a subdomain like `tunnel.` keeps your main domain free for a website.
You can also use the bare domain if you prefer.

---

## Step 1: DNS records

In your domain provider's DNS settings, add:

| Type | Name (host)  | Value          | Why |
|------|--------------|----------------|-----|
| `A`  | `tunnel`     | `203.0.113.10` | the address everyone connects to |
| `A`  | `*.tunnel`   | `203.0.113.10` | website tunnels like `mysite.tunnel.example.com` |

If your VPS has IPv6, you can add matching `AAAA` records, but this is
optional.

> ⚠️ **Cloudflare users:** set both records to **DNS only** (grey cloud), not
> **Proxied** (orange cloud). Cloudflare's proxy only forwards web traffic, so
> it would break Minecraft and everything else.

DNS can take a few minutes to update. Check it from your computer:

```sh
nslookup tunnel.example.com          # should show 203.0.113.10
nslookup test.tunnel.example.com     # should also show 203.0.113.10
```

---

## Step 2: Get the binary onto the VPS

On **your computer**, in this repo:

```sh
make dist
```

This builds binaries for every platform into `dist/`. Find out which one your
VPS needs by running this **on the VPS**:

```sh
uname -m
```

- `x86_64` → `warren-linux-amd64` (most VPSes)
- `aarch64` → `warren-linux-arm64` (Oracle Cloud Ampere, Hetzner ARM, Raspberry Pi)

Copy the binary and the service file from **your computer** to your home
folder on the VPS. Use the user you normally SSH in as: on Ubuntu VPSes
that's usually `ubuntu`, on Debian often `debian`, sometimes `root`.

```sh
scp dist/warren-linux-amd64 warren.service ubuntu@203.0.113.10:~/
```

> You can't `scp` straight into `/usr/local/bin` unless you log in as root.
> That gives `Permission denied`, which is why we upload to `~/` first.

Then SSH into the VPS (`ssh ubuntu@203.0.113.10`) and move the files into
place:

```sh
sudo mv ~/warren-linux-amd64 /usr/local/bin/warren
sudo chmod +x /usr/local/bin/warren
sudo mv ~/warren.service /etc/systemd/system/warren.service
sudo chown root:root /usr/local/bin/warren /etc/systemd/system/warren.service
warren version            # should print: warren 0.4.0
```

> **All remaining VPS commands in this guide need root.** Either put `sudo`
> in front of each one, or run `sudo -i` once to get a root shell (type
> `exit` to leave it when you're done).

---

## Step 3: Create a system user and config

warren runs as its own unprivileged user so it can't touch the rest of your
system:

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin warren
mkdir -p /etc/warren
```

Create the config file `/etc/warren/server.json` (for example with
`nano /etc/warren/server.json`):

```json
{
  "listen": ":7000",
  "domain": "tunnel.example.com",
  "http_addr": ":80",
  "port_min": 1024,
  "port_max": 65535,
  "users": []
}
```

What each line means:

- **`listen`**: the port warren clients connect to. `7000` is fine unless
  something else already uses it.
- **`domain`**: your domain from Step 1, without `http://`.
- **`http_addr`**: where website tunnels are served. `:80` is the normal HTTP
  port. If you already run a website on this VPS (nginx, Apache), see
  [Already running a web server?](#already-running-a-web-server-on-port-80)
  below. To disable website tunnels entirely, delete this line.
- **`port_min` / `port_max`**: which public ports your friends may use for
  game servers. With `1024`, people can grab the standard Minecraft ports
  (`25565`, `19132`). Ports already used by other programs on the VPS are
  skipped automatically.
- **`max_conns_per_tunnel`** (optional, not in the example): how many
  visitors one tunnel may have connected at once. The default, `1000`, is
  plenty for game servers and small sites; raise it for a busy website.
  Beyond the limit, new visitors are turned away (websites get a "too busy"
  page) and the log says so. Restart warren after changing it.
- **`users`**: leave it empty. The `warren adduser` command fills it in.

Lock it down so only warren can read it, because it will contain everyone's tokens:

```sh
chown -R warren:warren /etc/warren
chmod 700 /etc/warren
chmod 600 /etc/warren/server.json
```

---

## Step 4: Open the firewall

> ⚠️ **Allow SSH first.** If you enable a firewall without allowing SSH,
> you'll lock yourself out of your VPS.

With `ufw` (Ubuntu's default firewall):

```sh
ufw allow OpenSSH
ufw allow 7000/tcp            # warren control port
ufw allow 80/tcp              # website tunnels
ufw allow 1024:65535/tcp      # game server tunnels (TCP)
ufw allow 1024:65535/udp      # game server tunnels (UDP)
ufw enable
ufw status
```

**Your hosting provider may have its own firewall** in its web dashboard, in
addition to the one on the VPS. This applies to Oracle Cloud ("Security
Lists"), AWS ("Security Groups"), Google Cloud, Azure, and Hetzner Cloud
("Firewalls"). Open the same ports there too.

> **Oracle Cloud** images also come with strict `iptables` rules even when
> `ufw` is inactive. If connections time out, run:
> ```sh
> iptables -I INPUT -p tcp --dport 7000 -j ACCEPT
> iptables -I INPUT -p tcp --dport 80 -j ACCEPT
> iptables -I INPUT -p tcp --dport 1024:65535 -j ACCEPT
> iptables -I INPUT -p udp --dport 1024:65535 -j ACCEPT
> netfilter-persistent save
> ```

---

## Step 5: Start warren

```sh
systemctl daemon-reload
systemctl enable --now warren
systemctl status warren
```

You should see `active (running)`. Follow the live log with:

```sh
journalctl -u warren -f
```

which should show something like:

```
warren 0.4.0: control port :7000 (tls=false), domain tunnel.example.com, ports 1024-65535, 0 users
http tunnels on :80
```

Press `Ctrl+C` to stop watching the log. warren keeps running and starts
automatically after a reboot.

---

## Step 6: Add yourself and test it

Create your own user. Run it as the `warren` user so file ownership stays
correct (running as root also works; warren keeps the file's owner):

```sh
sudo -u warren warren adduser yourname
```

It prints something like:

```
Added user "yourname". Send them this command:

  warren login --server tunnel.example.com:7000 --token 3f9a...e1
```

On **your computer**, run that `login` command, then test with any local
service. For example, Python's built-in web server:

```sh
mkdir /tmp/hello && cd /tmp/hello && echo "it works" > index.html
python3 -m http.server 8000 &
warren http 8000 --subdomain hello
```

Open `http://hello.tunnel.example.com` in a browser. You should see "it works".
Press `Ctrl+C` to stop the tunnel.

🎉 The basic setup is done. The next step is optional but strongly recommended.

---

## Step 7 (recommended): Encrypt with TLS

Right now, tokens travel unencrypted between your friends and the VPS. Anyone
on the same café Wi-Fi could read them. TLS fixes that, and it's free with
Let's Encrypt.

Pick **one** of the two options below.

### Option A: any DNS provider (encrypts the control connection)

This gets a certificate for `tunnel.example.com`. Certbot needs port 80 for a
moment, which warren is using, so we tell certbot to pause warren while it
works:

```sh
apt install -y certbot
certbot certonly --standalone -d tunnel.example.com \
  --pre-hook "systemctl stop warren" \
  --post-hook "systemctl start warren"
```

Certbot remembers these hooks and auto-renews every ~60 days.

### Option B: Cloudflare DNS (also gives you HTTPS website tunnels)

A **wildcard** certificate (`*.tunnel.example.com`) covers the control
connection *and* lets every website tunnel use `https://`. Wildcards need DNS
verification. Here's how with Cloudflare; other big DNS providers have similar
certbot plugins (`apt search certbot-dns`).

1. In Cloudflare: **My Profile → API Tokens → Create Token → "Edit zone DNS"
   template**, limited to your domain. Copy the token.
2. On the VPS:
   ```sh
   apt install -y certbot python3-certbot-dns-cloudflare
   mkdir -p /root/.secrets
   echo "dns_cloudflare_api_token = PASTE_TOKEN_HERE" > /root/.secrets/cloudflare.ini
   chmod 600 /root/.secrets/cloudflare.ini

   certbot certonly --dns-cloudflare \
     --dns-cloudflare-credentials /root/.secrets/cloudflare.ini \
     -d tunnel.example.com -d '*.tunnel.example.com'
   ```
3. Open port 443: `ufw allow 443/tcp` (and in your provider's firewall).

### Give warren access to the certificate (both options)

Certbot's files are only readable by root, so we copy them into `/etc/warren`
every time they're renewed. Create
`/etc/letsencrypt/renewal-hooks/deploy/warren.sh`:

```sh
#!/bin/sh
mkdir -p /etc/warren/tls
cp /etc/letsencrypt/live/tunnel.example.com/fullchain.pem /etc/warren/tls/cert.pem
cp /etc/letsencrypt/live/tunnel.example.com/privkey.pem   /etc/warren/tls/key.pem
chown -R warren:warren /etc/warren/tls
chmod 600 /etc/warren/tls/key.pem
```

Make it executable and run it once now:

```sh
chmod +x /etc/letsencrypt/renewal-hooks/deploy/warren.sh
/etc/letsencrypt/renewal-hooks/deploy/warren.sh
```

warren notices renewed certificates by itself. You never need to restart it
for renewals.

### Turn TLS on in the config

Edit `/etc/warren/server.json` and add the `tls_cert` and `tls_key` lines:

```json
{
  "listen": ":7000",
  "domain": "tunnel.example.com",
  "tls_cert": "/etc/warren/tls/cert.pem",
  "tls_key": "/etc/warren/tls/key.pem",
  "http_addr": ":80",
  "port_min": 1024,
  "port_max": 65535,
  "users": [ ...leave as is... ]
}
```

If you did **Option B**, also add these lines for HTTPS website tunnels:

```json
  "https_addr": ":443",
  "https_cert": "/etc/warren/tls/cert.pem",
  "https_key": "/etc/warren/tls/key.pem",
```

Restart and check the log:

```sh
systemctl restart warren
journalctl -u warren -n 5        # should say tls=true
```

**Everyone must now add `--tls` when logging in.** Re-run your login with
`--tls` at the end. `warren adduser` adds it automatically from now on, so new
friends' commands are already correct. Existing users can keep their token;
they just re-run `warren login ... --tls`.

---

## Managing friends

All of these run on the VPS:

```sh
sudo -u warren warren adduser alice              # add, prints the login command to send
sudo -u warren warren adduser bob --max-tunnels 3  # limit simultaneous tunnels
sudo -u warren warren users                      # list everyone
sudo -u warren warren deluser alice              # revoke access
```

- New users work immediately. You don't need to restart anything.
- Deleted users are disconnected within 30 seconds.
- Send tokens privately (a DM, not a public channel). A token works like a
  password.
- If a token leaks, `deluser` and `adduser` the person again to issue a new
  one.

---

## Updating warren

On the VPS:

```sh
sudo warren update
```

This downloads the latest release from GitHub, checks it against the
release's checksums, replaces `/usr/local/bin/warren` and restarts the
service. `warren update --check` only tells you whether there's a new version.

(Versions before 0.3.0 don't have `warren update` yet. Update those once with
`setup.sh`, or as below.)

To install a build of your own instead, run `make dist` on your computer, then:

```sh
scp dist/warren-linux-amd64 ubuntu@203.0.113.10:~/warren.new
ssh -t ubuntu@203.0.113.10 'sudo install -m 755 -o root -g root ~/warren.new /usr/local/bin/warren && rm ~/warren.new && sudo systemctl restart warren'
```

Clients reconnect automatically within a few seconds. Players in a game are
usually not even kicked. Send friends the new client binary too if the
protocol changed. They'll get a clear "protocol version mismatch" error if it
did.

---

## Already running a web server on port 80?

If nginx (or similar) already uses port 80 on this VPS, give warren a
different HTTP port and let nginx forward tunnel traffic to it.

1. In `server.json`, set:
   ```json
   "http_addr": "127.0.0.1:8080",
   "public_http_port": 80,
   ```
   `http_addr` makes warren listen only locally on 8080. `public_http_port`
   makes the URLs it prints say port 80, because that's where nginx serves
   them.

2. Add an nginx site, e.g. `/etc/nginx/sites-enabled/warren`:
   ```nginx
   server {
       listen 80;
       server_name *.tunnel.example.com;
       location / {
           proxy_pass http://127.0.0.1:8080;
           proxy_http_version 1.1;
           proxy_set_header Host $host;
           proxy_set_header Upgrade $http_upgrade;
           proxy_set_header Connection "upgrade";
       }
   }
   ```
3. `nginx -t && systemctl reload nginx && systemctl restart warren`

---

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `systemctl status warren` shows **failed** | Run `journalctl -u warren -n 30`. Typical causes: a typo in `server.json` (check commas and quotes), or `address already in use`, meaning another program uses port 80/7000. |
| Client says `connect: connection timed out` | A firewall is blocking port 7000. Check `ufw status` **and** your provider's dashboard firewall (Step 4). |
| Client says `connection refused` | warren isn't running (`systemctl status warren`) or `listen` uses a different port. |
| Client says `invalid token` | Typo in the token, or the user was removed. Check `warren users`. |
| Client says `EOF (if the server uses TLS, add --tls)` | The server has TLS enabled but the client wasn't logged in with `--tls`. |
| Client says `certificate is valid for ..., not ...` | The client's `--server` doesn't match the certificate's domain. Use `tunnel.example.com`, not the IP. |
| Tunnel is "online" but players can't connect | The public port is blocked: check the firewall range (Step 4) and your provider's firewall. Test with `nc -vz tunnel.example.com PORT` from another machine. |
| `port N is outside the allowed range` | The friend asked for a port outside `port_min`–`port_max`. Widen the range or pick another port. |
| `port N is not available` | Another program on the VPS already uses that port (e.g. SSH on 22). Pick a different one. |
| Website tunnel shows `no tunnel here` | The wildcard DNS record `*.tunnel` is missing, or `domain` in the config doesn't match. |
| Added a user but they get `invalid token` | Make sure `/etc/warren/server.json` is readable by the warren user: `chown warren:warren /etc/warren/server.json`. |

---

## Uninstalling

```sh
systemctl disable --now warren
rm /etc/systemd/system/warren.service /usr/local/bin/warren
rm -r /etc/warren
rm -f /etc/letsencrypt/renewal-hooks/deploy/warren.sh
userdel warren
```
