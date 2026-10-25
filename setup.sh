#!/usr/bin/env bash
# warren server setup wizard.
#
# On the VPS:        wget https://raw.githubusercontent.com/proshiv85-byte/warren/main/setup.sh
#                    sudo bash setup.sh
#                    (downloads warren from the latest GitHub release, unless a
#                    warren-linux-* binary is next to this script)
#
# From your own PC:  ./setup.sh user@your-vps   (builds warren, copies it over,
#                                                and runs this wizard there)
#
# Other options:     -p PORT         SSH port of the VPS, if it isn't 22
#                    --binary PATH   use this warren binary instead of looking for one
#                    --uninstall     remove warren from this machine
#
# Running it again later lets you update the binary or change the settings;
# your users and their tokens are kept.

set -Eeuo pipefail

CONFIG_DIR=/etc/warren
CONFIG=$CONFIG_DIR/server.json
TLS_DIR=$CONFIG_DIR/tls
BIN_PATH=/usr/local/bin/warren
UNIT=/etc/systemd/system/warren.service
LE_HOOK=/etc/letsencrypt/renewal-hooks/deploy/warren.sh
ACME_ROOT=/var/lib/warren-acme
NGINX_MARK="# managed by warren setup.sh"
REPO=proshiv85-byte/warren

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# ---------------------------------------------------------------- helpers ---

if [[ -t 1 ]]; then
	B=$'\e[1m' DIM=$'\e[2m' RED=$'\e[31m' GRN=$'\e[32m' YEL=$'\e[33m' CYN=$'\e[36m' R=$'\e[0m'
else
	B='' DIM='' RED='' GRN='' YEL='' CYN='' R=''
fi

say()  { printf '%s\n' "$*"; }
info() { printf '%s==>%s %s\n' "$CYN" "$R" "$*"; }
ok()   { printf '%s ✓%s %s\n' "$GRN" "$R" "$*"; }
warn() { printf '%s !%s %s\n' "$YEL" "$R" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$RED" "$R" "$*" >&2; exit 1; }
header() { printf '\n%s── %s %s\n\n' "$B" "$*" "$R"; }

# Report unexpected failures, but not from inside $(...), where the caller
# handles the exit status.
DOWNLOAD_DIR=
trap '[[ -n $DOWNLOAD_DIR ]] && rm -rf "$DOWNLOAD_DIR"' EXIT
trap 'if ((BASH_SUBSHELL == 0)); then die "unexpected failure on line $LINENO (command: $BASH_COMMAND)"; fi' ERR

# Questions read from the terminal, so the script also works when piped.
TTY=/dev/tty

# ask "Question" [default] -> $REPLY
ask() {
	local q=$1 def=${2-}
	if [[ -n $def ]]; then
		printf '%s?%s %s %s[%s]%s ' "$B" "$R" "$q" "$DIM" "$def" "$R"
	else
		printf '%s?%s %s ' "$B" "$R" "$q"
	fi
	IFS= read -r REPLY <"$TTY" || die "no answer (is this running in a terminal?)"
	REPLY=${REPLY#"${REPLY%%[![:space:]]*}"}
	REPLY=${REPLY%"${REPLY##*[![:space:]]}"}
	[[ -z $REPLY ]] && REPLY=$def
	return 0
}

# ask_yn "Question" y|n -> exit status
ask_yn() {
	local q=$1 def=$2 hint
	[[ $def == y ]] && hint="Y/n" || hint="y/N"
	while true; do
		ask "$q ($hint)"
		case ${REPLY,,} in
			'') [[ $def == y ]]; return ;;
			y|yes) return 0 ;;
			n|no) return 1 ;;
			*) say "  Please answer y or n." ;;
		esac
	done
}

# ask_choice "Question" default option... -> $REPLY (1-based number)
ask_choice() {
	local q=$1 def=$2; shift 2
	local i=1 opt
	say "${B}?${R} $q"
	for opt in "$@"; do
		printf '    %s%d)%s %s\n' "$B" "$i" "$R" "$opt"
		i=$((i + 1))
	done
	while true; do
		ask "Choose" "$def"
		if [[ $REPLY =~ ^[0-9]+$ ]] && ((REPLY >= 1 && REPLY <= $#)); then
			return 0
		fi
		say "  Please enter a number from 1 to $#."
	done
}

ask_port() {
	local q=$1 def=$2
	while true; do
		ask "$q" "$def"
		if [[ $REPLY =~ ^[0-9]+$ ]] && ((REPLY >= 1 && REPLY <= 65535)); then
			return 0
		fi
		say "  That's not a port number (1-65535)."
	done
}

have() { command -v "$1" >/dev/null 2>&1; }

# Name of the program listening on a TCP port, or nothing if it's free.
port_owner() {
	local line
	line=$(ss -Hltnp "sport = :$1" 2>/dev/null | head -n1) || true
	[[ -z $line ]] && return 0
	if [[ $line =~ users:\(\(\"([^\"]+)\" ]]; then
		printf '%s\n' "${BASH_REMATCH[1]}"
	else
		printf 'something\n'
	fi
}

valid_domain() {
	[[ $1 =~ ^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$ ]]
}

valid_ipv4() {
	[[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
	local n
	for n in "${BASH_REMATCH[@]:1}"; do ((n <= 255)) || return 1; done
}

pkg_install() {
	if have apt-get; then
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null
	elif have dnf; then
		dnf install -y -q "$@" >/dev/null
	elif have yum; then
		yum install -y -q "$@" >/dev/null
	elif have pacman; then
		pacman -S --noconfirm --needed "$@" >/dev/null
	else
		return 1
	fi
}

APT_UPDATED=
pkg_refresh() {
	if have apt-get && [[ -z $APT_UPDATED ]]; then
		info "Updating the package list"
		apt-get update -qq >/dev/null || true
		APT_UPDATED=1
	fi
}

# ------------------------------------------------- run from your own PC ---

remote_setup() {
	local target=$1
	local ssh_opts=() scp_opts=()
	if [[ -n $SSH_PORT_ARG ]]; then
		ssh_opts=(-p "$SSH_PORT_ARG")
		scp_opts=(-P "$SSH_PORT_ARG")
	fi
	have ssh || die "ssh is not installed"
	have go || die "Go is needed to build warren (https://go.dev/dl/), or copy a binary over yourself and run setup.sh on the VPS"
	[[ -f $SCRIPT_DIR/go.mod ]] || die "run this from the warren source folder"

	info "Connecting to $target"
	local arch goarch
	arch=$(ssh "${ssh_opts[@]}" "$target" uname -m) || die "could not ssh into $target"
	case $arch in
		x86_64|amd64) goarch=amd64 ;;
		aarch64|arm64) goarch=arm64 ;;
		*) die "unsupported VPS architecture: $arch" ;;
	esac

	local tmp
	tmp=$(mktemp -d)
	REMOTE_TMP=$tmp
	trap 'rm -rf "$REMOTE_TMP"' EXIT
	info "Building warren for linux/$goarch"
	(cd "$SCRIPT_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch \
		go build -trimpath -ldflags "-s -w" -o "$tmp/warren" ./cmd/warren)
	cp "${BASH_SOURCE[0]}" "$tmp/setup.sh"

	info "Copying to $target:~/warren-setup/"
	ssh "${ssh_opts[@]}" "$target" 'rm -rf ~/warren-setup && mkdir ~/warren-setup'
	scp -q "${scp_opts[@]}" "$tmp/warren" "$tmp/setup.sh" "$target:warren-setup/"

	info "Starting the setup on the VPS"
	local sudo=sudo
	[[ $(ssh "${ssh_opts[@]}" "$target" id -u) == 0 ]] && sudo=
	ssh -t "${ssh_opts[@]}" "$target" "$sudo bash ~/warren-setup/setup.sh --binary ~/warren-setup/warren; status=\$?; rm -rf ~/warren-setup; exit \$status"
}

# ------------------------------------------------------- find a binary ---

# fetch URL FILE: download with curl or wget, whichever is installed.
fetch() {
	if have curl; then
		curl -fsSL --retry 2 -o "$2" "$1"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		return 1
	fi
}

# Downloads warren-linux-ARCH from the latest GitHub release into NEW_BIN,
# checking it against the release's SHA256SUMS.
download_release() {
	local goarch=$1 name="warren-linux-$1" dir
	local base="https://github.com/$REPO/releases/latest/download"
	have curl || have wget || { pkg_refresh; pkg_install curl; } ||
		die "need curl or wget to download warren"
	have sha256sum || die "sha256sum is missing (install coreutils)"

	info "Downloading $name from the latest release"
	dir=$(mktemp -d)
	DOWNLOAD_DIR=$dir
	fetch "$base/$name" "$dir/warren" && fetch "$base/SHA256SUMS" "$dir/SHA256SUMS" ||
		die "download failed. Check this VPS can reach github.com, or put $name next to this script"
	local want got
	want=$(awk -v n="$name" '$2 == n || $2 == "*" n {print $1}' "$dir/SHA256SUMS")
	got=$(sha256sum "$dir/warren" | awk '{print $1}')
	[[ -n $want ]] || die "the latest release has no $name (this VPS's architecture isn't supported)"
	[[ $want == "$got" ]] || die "checksum mismatch for $name: refusing to install it"
	ok "Downloaded and verified $name"
	NEW_BIN=$dir/warren
}

# find_binary [optional]: sets NEW_BIN to a warren binary to install. With
# "optional", an already installed warren is fine and nothing is downloaded.
find_binary() {
	local mode=${1-} arch goarch
	arch=$(uname -m)
	case $arch in
		x86_64|amd64) goarch=amd64 ;;
		aarch64|arm64) goarch=arm64 ;;
		*) goarch=$arch ;;
	esac

	if [[ -n ${BINARY:-} ]]; then
		[[ -f $BINARY ]] || die "$BINARY does not exist"
		NEW_BIN=$BINARY
	else
		local c
		for c in "$SCRIPT_DIR/warren-linux-$goarch" "$SCRIPT_DIR/dist/warren-linux-$goarch" \
			"$SCRIPT_DIR/warren" "$PWD/warren-linux-$goarch" "$PWD/warren"; do
			if [[ -f $c && ! -d $c ]]; then
				NEW_BIN=$c
				break
			fi
		done
	fi

	if [[ -z ${NEW_BIN:-} && -f $SCRIPT_DIR/go.mod ]] && have go; then
		info "Building warren with Go"
		NEW_BIN=$(mktemp)
		(cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$NEW_BIN" ./cmd/warren)
	fi

	if [[ -z ${NEW_BIN:-} ]]; then
		if [[ $mode == optional && -x $BIN_PATH ]]; then
			return 0
		fi
		download_release "$goarch"
	fi

	chmod +x "$NEW_BIN"
	"$NEW_BIN" version >/dev/null 2>&1 ||
		die "$NEW_BIN doesn't run on this machine (wrong architecture? this VPS is $arch, it needs warren-linux-$goarch)"
}

install_binary() {
	[[ -z ${NEW_BIN:-} ]] && return 0
	install -m 755 -o root -g root "$NEW_BIN" "$BIN_PATH.new"
	mv -f "$BIN_PATH.new" "$BIN_PATH"
	ok "Installed $("$BIN_PATH" version) to $BIN_PATH"
}

# ------------------------------------------------------------- uninstall ---

uninstall() {
	header "Uninstall warren"
	ask_yn "Remove warren, its config and all user tokens from this machine?" n || exit 0
	systemctl disable --now warren >/dev/null 2>&1 || true
	rm -f "$UNIT" "$BIN_PATH" "$LE_HOOK"
	systemctl daemon-reload
	rm -rf "$CONFIG_DIR" "$ACME_ROOT"
	local f
	for f in /etc/nginx/sites-enabled/warren /etc/nginx/sites-available/warren /etc/nginx/conf.d/warren.conf; do
		if [[ -f $f ]] && grep -qF "$NGINX_MARK" "$f"; then
			rm -f "$f"
			NGINX_CHANGED=1
		fi
	done
	if [[ -n ${NGINX_CHANGED:-} ]] && nginx -t >/dev/null 2>&1; then
		systemctl reload nginx || true
	fi
	userdel warren 2>/dev/null || true
	ok "warren is removed."
	say "  Firewall rules and Let's Encrypt certificates were left in place."
	say "  Remove a certificate with: certbot delete --cert-name YOUR_DOMAIN"
}

# ------------------------------------------------------------- questions ---

detect_ip() {
	local ip=
	if have curl; then
		ip=$(curl -4 -fsS --max-time 5 https://api.ipify.org 2>/dev/null ||
			curl -4 -fsS --max-time 5 https://ifconfig.me 2>/dev/null) || true
	elif have wget; then
		ip=$(wget -4 -qO- --timeout=5 https://api.ipify.org 2>/dev/null) || true
	fi
	if ! valid_ipv4 "${ip:-}"; then
		ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}') || true
	fi
	if valid_ipv4 "${ip:-}"; then printf '%s\n' "$ip"; fi
}

# Prints the IPv4 addresses a name resolves to, one per line.
resolve() {
	getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u || true
}

dns_matches() {
	resolve "$1" | grep -xF "$PUBLIC_IP" >/dev/null
}

check_dns() {
	local need_wild=$1 base_ok wild_ok
	while true; do
		base_ok=y wild_ok=y
		dns_matches "$DOMAIN" || base_ok=
		if [[ -n $need_wild ]]; then
			dns_matches "warren-check.$DOMAIN" || wild_ok=
		fi
		if [[ -n $base_ok && -n $wild_ok ]]; then
			ok "DNS for $DOMAIN points to this VPS"
			return 0
		fi

		warn "DNS is not set up yet (or hasn't updated)."
		say ""
		say "  In your domain provider's DNS settings, add:"
		say ""
		printf '    %-6s %-30s %s\n' Type Name Value
		[[ -z $base_ok ]] && printf '    %-6s %-30s %s\n' A "$DOMAIN" "$PUBLIC_IP"
		[[ -z $wild_ok ]] && printf '    %-6s %-30s %s\n' A "*.$DOMAIN" "$PUBLIC_IP"
		say ""
		say "  Cloudflare users: set them to \"DNS only\" (grey cloud), not \"Proxied\"."
		local now
		now=$(resolve "$DOMAIN" | paste -sd ' ' -) || true
		[[ -n $now ]] && say "  Right now $DOMAIN points to: $now"
		say ""
		ask_choice "What now?" 1 "Check again (after you've added the records)" \
			"Continue anyway" "Quit"
		case $REPLY in
			1) ;;
			2) DNS_OK=; return 0 ;;
			3) exit 0 ;;
		esac
	done
}

ask_questions() {
	header "Your server's address"

	PUBLIC_IP=$(detect_ip) || true
	while true; do
		ask "This VPS's public IPv4 address" "${PUBLIC_IP:-}"
		valid_ipv4 "$REPLY" && break
		say "  That's not an IPv4 address like 203.0.113.10."
	done
	PUBLIC_IP=$REPLY

	say ""
	say "  With a domain (like tunnel.example.com) friends connect to a name instead"
	say "  of an IP, you get real TLS certificates, and you can share websites"
	say "  (https://name.tunnel.example.com). Without one, everything except website"
	say "  tunnels still works."
	say ""
	DNS_OK=y
	if ask_yn "Do you have a domain you want to use for warren?" n; then
		HAS_DOMAIN=y
		while true; do
			ask "Domain or subdomain for warren (e.g. tunnel.example.com)" "${OLD_DOMAIN:-}"
			REPLY=${REPLY,,}
			REPLY=${REPLY#http://}
			REPLY=${REPLY#https://}
			REPLY=${REPLY%/}
			valid_domain "$REPLY" && break
			say "  That doesn't look like a domain name."
		done
		DOMAIN=$REPLY
	else
		HAS_DOMAIN=
		DOMAIN=$PUBLIC_IP
	fi

	header "Ports"
	while true; do
		ask_port "Control port (the port warren clients connect to)" "${OLD_CONTROL_PORT:-7000}"
		local owner
		owner=$(port_owner "$REPLY")
		if [[ -n $owner && $owner != warren ]]; then
			say "  Port $REPLY is already used by $owner. Pick another one."
			continue
		fi
		break
	done
	CONTROL_PORT=$REPLY

	say ""
	say "  Friends pick public ports for their game servers from a range. The"
	say "  default allows everything from 1024 up, including Minecraft's 25565."
	while true; do
		ask_port "Lowest port friends may use" "${OLD_PORT_MIN:-1024}"
		PORT_MIN=$REPLY
		ask_port "Highest port friends may use" "${OLD_PORT_MAX:-65535}"
		PORT_MAX=$REPLY
		if ((PORT_MIN > PORT_MAX)); then
			say "  The lowest port must not be above the highest."
			continue
		fi
		if ((PORT_MIN <= SSH_PORT && SSH_PORT <= PORT_MAX)); then
			warn "Your SSH port ($SSH_PORT) is inside that range. warren skips ports in use, so that's OK."
		fi
		break
	done

	# Website tunnels need wildcard subdomains, so only with a domain.
	WEB=
	WEB_ACME=
	HTTP_ADDR=
	PUBLIC_HTTP_PORT=
	USE_NGINX=
	local p80
	p80=$(port_owner 80)
	if [[ -n $HAS_DOMAIN ]]; then
		header "Website tunnels"
		say "  Website tunnels let you share a local web app as"
		say "  http://NAME.$DOMAIN. They need a second DNS record: *.$DOMAIN."
		say ""
		if ask_yn "Enable website tunnels?" y; then
			WEB=y
			if [[ -z $p80 || $p80 == warren ]]; then
				HTTP_ADDR=":80"
			elif [[ $p80 == nginx ]]; then
				say ""
				say "  nginx already uses port 80. warren will listen on 127.0.0.1:8080 and"
				say "  nginx will forward *.$DOMAIN to it."
				if ask_yn "Add an nginx site for that?" y; then
					HTTP_ADDR="127.0.0.1:8080"
					PUBLIC_HTTP_PORT=80
					USE_NGINX=y
				else
					WEB=
				fi
			else
				warn "Port 80 is used by $p80, so website tunnels can't use it."
				say "  Stop $p80 and run this setup again, or see docs/INSTALL.md"
				say "  (\"Already running a web server on port 80?\")."
				WEB=
			fi
		fi
	fi

	header "Encryption"
	say "  TLS encrypts the connection between warren clients and this server, so"
	say "  nobody on the same Wi-Fi can read your friends' tokens."
	say ""
	local opts=() keys=()
	if [[ -n $HAS_DOMAIN ]]; then
		if [[ -z $p80 || $p80 == warren || $p80 == nginx ]]; then
			opts+=("Let's Encrypt certificate (free, recommended)")
			keys+=(le)
		fi
		if [[ -n $WEB ]] && have apt-get && [[ -z $(port_owner 443) ]]; then
			opts+=("Let's Encrypt wildcard via Cloudflare DNS (also gives https:// website tunnels)")
			keys+=(cloudflare)
		fi
	fi
	opts+=("Self-signed certificate (encrypted; clients skip the certificate check)")
	keys+=(self)
	opts+=("No encryption")
	keys+=(none)
	ask_choice "How should warren encrypt connections?" 1 "${opts[@]}"
	TLS_MODE=${keys[REPLY - 1]}

	CF_TOKEN=
	if [[ $TLS_MODE == cloudflare ]]; then
		say ""
		say "  Create a Cloudflare API token: My Profile → API Tokens → Create Token →"
		say "  \"Edit zone DNS\" template, limited to your domain."
		while [[ -z $CF_TOKEN ]]; do
			ask "Cloudflare API token"
			CF_TOKEN=$REPLY
		done
	fi
	# Without a wildcard, warren can get a certificate per website tunnel itself.
	local p443
	p443=$(port_owner 443)
	if [[ -n $WEB && $TLS_MODE != cloudflare ]] && [[ -z $p443 || $p443 == warren ]]; then
		say ""
		say "  warren can also get an https:// certificate for each website tunnel"
		say "  by itself, from Let's Encrypt, with no DNS setup. It uses port 443."
		if ask_yn "Give website tunnels https:// addresses?" y; then
			WEB_ACME=y
		fi
	fi
	# Let's Encrypt's port 80 check goes through nginx when nginx owns port 80.
	if [[ $TLS_MODE == le && $p80 == nginx ]]; then
		USE_NGINX=y
	fi

	if [[ -n $HAS_DOMAIN ]]; then
		header "Checking DNS"
		check_dns "$WEB"
	fi
}

confirm() {
	header "Summary"
	printf '  %-22s %s\n' "Address" "$DOMAIN"
	printf '  %-22s %s\n' "Control port" "$CONTROL_PORT"
	printf '  %-22s %s\n' "Game ports" "$PORT_MIN-$PORT_MAX (TCP and UDP)"
	if [[ -n $WEB ]]; then
		local scheme=http
		[[ $TLS_MODE == cloudflare || -n $WEB_ACME ]] && scheme=https
		printf '  %-22s %s\n' "Website tunnels" "$scheme://NAME.$DOMAIN"
	else
		printf '  %-22s %s\n' "Website tunnels" "off"
	fi
	case $TLS_MODE in
		le) printf '  %-22s %s\n' "Encryption" "Let's Encrypt" ;;
		cloudflare) printf '  %-22s %s\n' "Encryption" "Let's Encrypt wildcard (Cloudflare)" ;;
		self) printf '  %-22s %s\n' "Encryption" "self-signed certificate" ;;
		none) printf '  %-22s %s\n' "Encryption" "none" ;;
	esac
	[[ -n $USE_NGINX ]] && printf '  %-22s %s\n' "nginx" "adds a site for $DOMAIN"
	say ""
	ask_yn "Set it up like this?" y || exit 0
}

# --------------------------------------------------------------- install ---

create_user() {
	if ! id warren >/dev/null 2>&1; then
		local nologin=/usr/sbin/nologin
		[[ -x $nologin ]] || nologin=/sbin/nologin
		useradd --system --no-create-home --shell "$nologin" warren
		ok "Created system user warren"
	fi
	mkdir -p "$CONFIG_DIR"
	chown warren:warren "$CONFIG_DIR"
	chmod 700 "$CONFIG_DIR"
}

write_unit() {
	cat >"$UNIT" <<-'EOF'
	[Unit]
	Description=warren tunnel server
	After=network-online.target
	Wants=network-online.target

	[Service]
	User=warren
	WorkingDirectory=/etc/warren
	ExecStart=/usr/local/bin/warren server --config /etc/warren/server.json
	Restart=always
	RestartSec=2
	# allow binding ports below 1024 (80, 443, ...) without running as root
	AmbientCapabilities=CAP_NET_BIND_SERVICE
	LimitNOFILE=65535

	[Install]
	WantedBy=multi-user.target
	EOF
	systemctl daemon-reload
}

write_nginx_site() {
	local names=$DOMAIN
	[[ -n $WEB ]] && names="$DOMAIN *.$DOMAIN"
	local file
	if [[ -d /etc/nginx/sites-available ]]; then
		file=/etc/nginx/sites-available/warren
	else
		file=/etc/nginx/conf.d/warren.conf
	fi
	mkdir -p "$ACME_ROOT"
	{
		say "$NGINX_MARK"
		say "server {"
		say "    listen 80;"
		say "    listen [::]:80;"
		say "    server_name $names;"
		say ""
		say "    location /.well-known/acme-challenge/ {"
		say "        root $ACME_ROOT;"
		say "    }"
		if [[ -n $WEB ]]; then
			say ""
			say "    location / {"
			say "        proxy_pass http://127.0.0.1:8080;"
			say "        proxy_http_version 1.1;"
			say "        proxy_set_header Host \$host;"
			say "        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;"
			say "        proxy_set_header X-Forwarded-Proto \$scheme;"
			say "        proxy_set_header Upgrade \$http_upgrade;"
			say "        proxy_set_header Connection \"upgrade\";"
			say "    }"
		fi
		say "}"
	} >"$file"
	[[ $file == /etc/nginx/sites-available/warren ]] && ln -sf "$file" /etc/nginx/sites-enabled/warren
	if ! nginx -t >/dev/null 2>&1; then
		rm -f "$file" /etc/nginx/sites-enabled/warren
		nginx -t || true
		warn "nginx rejected the new site, so it was removed."
		return 1
	fi
	systemctl reload nginx
	ok "Added nginx site $file"
}

install_le_hook() {
	mkdir -p "$(dirname "$LE_HOOK")"
	cat >"$LE_HOOK" <<-EOF
	#!/bin/sh
	# Copies the renewed certificate where warren can read it. warren picks
	# it up by itself, no restart needed.
	set -e
	mkdir -p $TLS_DIR
	cp /etc/letsencrypt/live/$DOMAIN/fullchain.pem $TLS_DIR/cert.pem
	cp /etc/letsencrypt/live/$DOMAIN/privkey.pem $TLS_DIR/key.pem
	chown -R warren:warren $TLS_DIR
	chmod 600 $TLS_DIR/key.pem
	EOF
	chmod 755 "$LE_HOOK"
	"$LE_HOOK"
}

ensure_certbot() {
	local pkgs=(certbot)
	[[ $1 == cloudflare ]] && pkgs+=(python3-certbot-dns-cloudflare)
	if have certbot && [[ $1 != cloudflare ]]; then
		return 0
	fi
	info "Installing certbot"
	pkg_refresh
	if ! pkg_install "${pkgs[@]}"; then
		# RHEL-likes ship certbot in EPEL.
		if have dnf && pkg_install epel-release && pkg_install "${pkgs[@]}"; then
			return 0
		fi
		return 1
	fi
}

get_le_cert() {
	ensure_certbot "$TLS_MODE" || { warn "Could not install certbot."; return 1; }
	local args=(certonly --non-interactive --agree-tos --register-unsafely-without-email
		--keep-until-expiring --cert-name "$DOMAIN" -d "$DOMAIN")

	if [[ $TLS_MODE == cloudflare ]]; then
		mkdir -p /root/.secrets
		install -m 600 /dev/null /root/.secrets/cloudflare.ini
		printf 'dns_cloudflare_api_token = %s\n' "$CF_TOKEN" >/root/.secrets/cloudflare.ini
		args+=(-d "*.$DOMAIN" --dns-cloudflare --dns-cloudflare-credentials /root/.secrets/cloudflare.ini)
	elif [[ -n $USE_NGINX ]]; then
		args+=(--webroot -w "$ACME_ROOT")
	else
		args+=(--standalone)
		# Certbot needs port 80 for a moment; if warren uses it, pause warren.
		if [[ $HTTP_ADDR == ":80" ]]; then
			args+=(--pre-hook "systemctl stop warren" --post-hook "systemctl start warren")
		fi
	fi

	info "Requesting a certificate from Let's Encrypt"
	if ! certbot "${args[@]}"; then
		warn "Let's Encrypt could not issue a certificate."
		if [[ -z $DNS_OK ]]; then
			say "  Your DNS records weren't pointing here yet, which is the usual cause."
		else
			say "  Check that port 80 is open in your VPS provider's firewall."
		fi
		return 1
	fi
	install_le_hook
	ok "Got a certificate for $DOMAIN (renews automatically)"
}

make_self_signed() {
	local check=(-checkip "$PUBLIC_IP")
	[[ -n $HAS_DOMAIN ]] && check=(-checkhost "$DOMAIN")
	if [[ -f $TLS_DIR/cert.pem && -f $TLS_DIR/key.pem ]] && [[ -z ${OLD_TLS_LE:-} ]] &&
		openssl x509 -in "$TLS_DIR/cert.pem" -noout -checkend 2592000 >/dev/null 2>&1 &&
		openssl x509 -in "$TLS_DIR/cert.pem" -noout "${check[@]}" 2>/dev/null | grep -v NOT >/dev/null; then
		ok "Keeping the existing self-signed certificate"
		return 0
	fi
	have openssl || { pkg_refresh; pkg_install openssl; } || { warn "openssl is not available."; return 1; }
	mkdir -p "$TLS_DIR"
	local san="IP:$PUBLIC_IP"
	[[ -n $HAS_DOMAIN ]] && san="DNS:$DOMAIN,$san"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
		-subj "/CN=$DOMAIN" -addext "subjectAltName=$san" \
		-keyout "$TLS_DIR/key.pem" -out "$TLS_DIR/cert.pem" 2>/dev/null
	rm -f "$LE_HOOK"
	chown -R warren:warren "$TLS_DIR"
	chmod 600 "$TLS_DIR/key.pem"
	ok "Created a self-signed certificate (valid 10 years)"
}

setup_tls() {
	if [[ $TLS_MODE == le || $TLS_MODE == cloudflare ]]; then
		if [[ -n $USE_NGINX ]] && ! write_nginx_site; then
			[[ $TLS_MODE == le ]] && TLS_MODE=failed
			[[ -n $WEB && $HTTP_ADDR == 127.0.0.1:8080 ]] && WEB= HTTP_ADDR= PUBLIC_HTTP_PORT=
		fi
		if [[ $TLS_MODE != failed ]] && get_le_cert; then
			return 0
		fi
		ask_choice "Continue with:" 1 "A self-signed certificate instead" "No encryption"
		[[ $REPLY == 1 ]] && TLS_MODE=self || TLS_MODE=none
	elif [[ -n $USE_NGINX ]]; then
		write_nginx_site || { WEB= HTTP_ADDR= PUBLIC_HTTP_PORT=; }
	fi
	if [[ $TLS_MODE == self ]]; then
		make_self_signed || TLS_MODE=none
	fi
	if [[ $TLS_MODE == none ]]; then
		rm -f "$LE_HOOK"
	fi
}

# The "users" array from the existing config, as written by warren itself.
old_users_json() {
	[[ -f $CONFIG ]] || { echo "[]"; return; }
	awk '
		/^  "users": (\[\]|null),?$/ { print "[]"; found = 1; exit }
		/^  "users": \[$/ { f = 1; print "["; next }
		f && /^  \],?$/ { print "  ]"; found = 1; exit }
		f { print }
		END { if (!found) print "[]" }
	' "$CONFIG"
}

write_config() {
	local tmp=$CONFIG.new users
	users=$(old_users_json)
	local lines=()
	lines+=("\"listen\": \":$CONTROL_PORT\"")
	lines+=("\"domain\": \"$DOMAIN\"")
	if [[ $TLS_MODE != none ]]; then
		lines+=("\"tls_cert\": \"$TLS_DIR/cert.pem\"" "\"tls_key\": \"$TLS_DIR/key.pem\"")
	fi
	[[ -n $WEB && -n $HTTP_ADDR ]] && lines+=("\"http_addr\": \"$HTTP_ADDR\"")
	[[ -n $WEB && -n $PUBLIC_HTTP_PORT ]] && lines+=("\"public_http_port\": $PUBLIC_HTTP_PORT")
	# nginx in front: believe the visitor IPs it passes along.
	[[ -n $WEB && $HTTP_ADDR == 127.0.0.1:* ]] && lines+=("\"trusted_proxies\": [\"127.0.0.1\"]")
	if [[ -n $WEB_ACME ]]; then
		lines+=("\"https_addr\": \":443\"" "\"acme\": true" "\"https_redirect\": true")
	fi
	if [[ -n $WEB && $TLS_MODE == cloudflare ]]; then
		lines+=("\"https_addr\": \":443\"" "\"https_cert\": \"$TLS_DIR/cert.pem\"" "\"https_key\": \"$TLS_DIR/key.pem\"")
	fi
	lines+=("\"port_min\": $PORT_MIN" "\"port_max\": $PORT_MAX")
	lines+=("\"users\": $users")

	{
		say "{"
		local i
		for ((i = 0; i < ${#lines[@]}; i++)); do
			if ((i < ${#lines[@]} - 1)); then
				say "  ${lines[i]},"
			else
				say "  ${lines[i]}"
			fi
		done
		say "}"
	} >"$tmp"
	chown warren:warren "$tmp"
	chmod 600 "$tmp"

	# Make sure the new file is valid and kept every user before replacing the old one.
	"$BIN_PATH" users --config "$tmp" >/dev/null || { rm -f "$tmp"; die "the new config is invalid (bug in setup.sh?)"; }
	if [[ -f $CONFIG ]]; then
		local before after
		before=$("$BIN_PATH" users --config "$CONFIG" 2>/dev/null) || before=
		after=$("$BIN_PATH" users --config "$tmp")
		if [[ -n $before && $before != "$after" ]]; then
			rm -f "$tmp"
			die "could not carry the existing users over; $CONFIG was left unchanged"
		fi
		cp -p "$CONFIG" "$CONFIG.bak"
	fi
	mv -f "$tmp" "$CONFIG"
	ok "Wrote $CONFIG"
}

open_firewall() {
	header "Firewall"
	local tcp=("$CONTROL_PORT") range="$PORT_MIN:$PORT_MAX"
	# Port 80 for website tunnels (directly or through nginx) and for
	# Let's Encrypt's checks when the certificate is issued and renewed.
	[[ -n $WEB || $TLS_MODE == le ]] && tcp+=(80)
	[[ -n $WEB && ($TLS_MODE == cloudflare || -n $WEB_ACME) ]] && tcp+=(443)

	local done_fw=
	if have ufw && ufw status 2>/dev/null | grep "Status: active" >/dev/null; then
		local p
		for p in "${tcp[@]}"; do ufw allow "$p/tcp" >/dev/null; done
		ufw allow "$range/tcp" >/dev/null
		ufw allow "$range/udp" >/dev/null
		ok "Opened the ports in ufw"
		done_fw=y
	elif have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then
		local p
		for p in "${tcp[@]}"; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
		firewall-cmd --permanent --add-port="$PORT_MIN-$PORT_MAX/tcp" >/dev/null
		firewall-cmd --permanent --add-port="$PORT_MIN-$PORT_MAX/udp" >/dev/null
		firewall-cmd --reload >/dev/null
		ok "Opened the ports in firewalld"
		done_fw=y
	elif have iptables && iptables -S INPUT 2>/dev/null | grep -E -- '-j (REJECT|DROP)|-P INPUT DROP' >/dev/null; then
		# Oracle Cloud images and similar ship with a strict iptables setup.
		say "  This VPS has iptables rules that block incoming connections."
		if ask_yn "Add iptables rules to let warren's ports through?" y; then
			local p
			for p in "${tcp[@]}"; do
				iptables -C INPUT -p tcp --dport "$p" -j ACCEPT 2>/dev/null ||
					iptables -I INPUT -p tcp --dport "$p" -j ACCEPT
			done
			local proto
			for proto in tcp udp; do
				iptables -C INPUT -p "$proto" --dport "$range" -j ACCEPT 2>/dev/null ||
					iptables -I INPUT -p "$proto" --dport "$range" -j ACCEPT
			done
			if have netfilter-persistent; then
				netfilter-persistent save >/dev/null 2>&1 && ok "Saved the iptables rules"
			else
				warn "Rules added, but they'll be lost on reboot. Install iptables-persistent to keep them."
			fi
			done_fw=y
		fi
	elif have ufw; then
		say "  ufw is installed but turned off, so nothing on this VPS blocks warren."
		if ask_yn "Turn ufw on (allowing SSH on port $SSH_PORT and warren's ports)?" n; then
			ufw allow "$SSH_PORT/tcp" >/dev/null
			local p
			for p in "${tcp[@]}"; do ufw allow "$p/tcp" >/dev/null; done
			ufw allow "$range/tcp" >/dev/null
			ufw allow "$range/udp" >/dev/null
			ufw --force enable >/dev/null
			ok "ufw is on"
			done_fw=y
		fi
	else
		ok "No firewall is running on this VPS"
	fi

	say ""
	say "  ${YEL}Your hosting provider may have its own firewall${R} in its web dashboard"
	say "  (Oracle \"Security Lists\", AWS \"Security Groups\", Hetzner/GCP/Azure"
	say "  \"Firewalls\"). Open these ports there too:"
	say "    TCP ${tcp[*]}, and TCP+UDP $PORT_MIN-$PORT_MAX"
}

start_service() {
	header "Starting warren"
	systemctl enable warren >/dev/null 2>&1
	systemctl restart warren
	local i
	for i in 1 2 3 4 5 6; do
		sleep 1
		systemctl is-active --quiet warren && [[ -n $(port_owner "$CONTROL_PORT") ]] && break
	done
	if ! systemctl is-active --quiet warren; then
		journalctl -u warren -n 20 --no-pager >&2 || true
		die "warren did not start (log above)"
	fi
	ok "warren is running and starts automatically after a reboot"
	journalctl -u warren -n 3 --no-pager -o cat 2>/dev/null | sed 's/^/    /' || true
}

first_user() {
	local count
	count=$("$BIN_PATH" users --config "$CONFIG" | wc -l)
	header "Users"
	if ((count > 0)); then
		ok "Kept $count existing user(s). Their tokens still work."
		[[ $TLS_CHANGED ]] && warn "Encryption changed: everyone must log in again (see below)."
		ask_yn "Add another user now?" n || return 0
	fi
	local name
	while true; do
		ask "Name for your own user" "${SUDO_USER:-me}"
		name=$REPLY
		[[ $name =~ ^[A-Za-z0-9._-]{1,32}$ ]] || { say "  Use letters, digits, dot, dash or underscore."; continue; }
		break
	done
	local out
	if out=$(runuser -u warren -- "$BIN_PATH" adduser "$name" --config "$CONFIG" 2>&1); then
		LOGIN_CMD=$(printf '%s\n' "$out" | grep -o 'warren login .*' | head -n1)
	else
		warn "$out"
	fi
}

finish() {
	header "Done"
	if [[ -n ${LOGIN_CMD:-} ]]; then
		say "  On your own PC, log in with:"
		say ""
		say "    ${B}$LOGIN_CMD${R}"
		say ""
		say "  In Minecraft with the warren mod: type /warren login and paste that"
		say "  whole line into the Server address box."
		say ""
	fi
	if [[ ${TLS_CHANGED:-} && -z ${LOGIN_CMD:-} ]]; then
		say "  Encryption changed, so existing users must run their warren login"
		say "  command again with the right flags:"
		case $TLS_MODE in
			le|cloudflare) say "    add --tls" ;;
			self) say "    add --tls --fingerprint $("$BIN_PATH" fingerprint --config "$CONFIG" 2>/dev/null || echo '(run: warren fingerprint)')" ;;
			none) say "    remove --tls, --insecure and --fingerprint" ;;
		esac
		say ""
	fi
	say "  Manage friends (on this VPS):"
	say "    sudo -u warren warren adduser NAME     add someone, prints their login command"
	say "    sudo -u warren warren users            list everyone"
	say "    sudo -u warren warren deluser NAME     revoke access"
	say ""
	say "  Logs:     journalctl -u warren -f"
	say "  Re-run this setup any time to update warren or change settings."
	if [[ -n $HAS_DOMAIN && -z $DNS_OK ]]; then
		say ""
		warn "Remember to add the DNS records for $DOMAIN shown earlier."
	fi
	if [[ $TLS_MODE == none ]]; then
		say ""
		warn "Tokens are sent unencrypted. Re-run setup to add encryption later."
	fi
}

load_old_settings() {
	[[ -f $CONFIG ]] || return 0
	local listen
	listen=$(sed -nE 's/^  "listen": "[^"]*:([0-9]+)",?$/\1/p' "$CONFIG")
	OLD_CONTROL_PORT=$listen
	OLD_DOMAIN=$(sed -nE 's/^  "domain": "([^"]*)",?$/\1/p' "$CONFIG")
	valid_ipv4 "${OLD_DOMAIN:-}" && OLD_DOMAIN=
	OLD_PORT_MIN=$(sed -nE 's/^  "port_min": ([0-9]+),?$/\1/p' "$CONFIG")
	OLD_PORT_MAX=$(sed -nE 's/^  "port_max": ([0-9]+),?$/\1/p' "$CONFIG")
	if grep -q '"tls_cert"' "$CONFIG"; then
		if [[ -f $LE_HOOK ]]; then OLD_TLS=le OLD_TLS_LE=y; else OLD_TLS=self; fi
	else
		OLD_TLS=none
	fi
}

# ------------------------------------------------------------------ main ---

main() {
	BINARY=
	SSH_PORT_ARG=
	local target=
	while (($#)); do
		case $1 in
			--binary) BINARY=${2:?--binary needs a path}; shift 2 ;;
			--binary=*) BINARY=${1#*=}; shift ;;
			--uninstall) UNINSTALL=y; shift ;;
			-p) SSH_PORT_ARG=${2:?-p needs a port}; shift 2 ;;
			-h|--help) sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
			*@*) target=$1; shift ;;
			*) die "unknown option $1 (see --help)" ;;
		esac
	done

	if [[ -n $target ]]; then
		remote_setup "$target"
		exit
	fi

	[[ $(uname -s) == Linux ]] || die "run this on your Linux VPS, or from your PC as: ./setup.sh user@your-vps"
	if ((EUID != 0)); then
		have sudo || die "run this as root"
		[[ -f ${BASH_SOURCE[0]} ]] || die "run it as root, e.g. curl -fsSL URL | sudo bash"
		exec sudo bash "${BASH_SOURCE[0]}" ${BINARY:+--binary "$BINARY"} ${UNINSTALL:+--uninstall}
	fi
	have systemctl || die "this setup needs systemd"
	have ss || { pkg_refresh; pkg_install iproute2; } || die "the ss command (iproute2) is missing"

	say ""
	say "${B}warren server setup${R}"

	if [[ -n ${UNINSTALL:-} ]]; then
		uninstall
		exit 0
	fi

	SSH_PORT=$(sshd -T 2>/dev/null | awk '$1 == "port" {print $2; exit}') || true
	SSH_PORT=${SSH_PORT:-22}

	if [[ -f $CONFIG ]]; then
		load_old_settings
		header "warren is already set up here"
		ask_choice "What do you want to do?" 1 \
			"Update the warren program and restart (keeps all settings)" \
			"Change the settings (keeps users and tokens)" \
			"Add a user" \
			"Uninstall warren" \
			"Quit"
		case $REPLY in
			1)
				find_binary
				install_binary
				write_unit
				systemctl restart warren
				ok "Restarted warren. Clients reconnect by themselves."
				exit 0
				;;
			2) find_binary optional ;;
			3)
				LOGIN_CMD=
				TLS_CHANGED=
				first_user_forced
				exit 0
				;;
			4) uninstall; exit 0 ;;
			5) exit 0 ;;
		esac
	else
		find_binary
	fi

	ask_questions
	confirm

	header "Installing"
	install_binary
	create_user
	write_unit
	open_firewall
	header "Encryption"
	setup_tls
	TLS_CHANGED=
	if [[ -n ${OLD_TLS:-} ]]; then
		local new=$TLS_MODE
		[[ $new == cloudflare ]] && new=le
		[[ $new != "$OLD_TLS" ]] && TLS_CHANGED=y
	fi
	write_config
	start_service
	first_user
	finish
}

# Adding a user from the "already set up" menu.
first_user_forced() {
	local name out
	while true; do
		ask "Name for the new user"
		name=$REPLY
		[[ $name =~ ^[A-Za-z0-9._-]{1,32}$ ]] || { say "  Use letters, digits, dot, dash or underscore."; continue; }
		break
	done
	out=$(runuser -u warren -- "$BIN_PATH" adduser "$name" --config "$CONFIG" 2>&1) || die "$out"
	printf '%s\n' "$out"
}

main "$@"
