// warren is a small self-hosted tunnel server and client: expose local
// TCP, UDP and HTTP services through a VPS you control.
package main

import (
	"fmt"
	"os"
)

// version is a var so builds can override it: -ldflags "-X main.version=..."
var version = "0.6.0"

const usageText = `warren - self-hosted tunnels (TCP, UDP, HTTP)

Client:
  warren login --server HOST[:PORT] --token TOKEN [--tls] [--fingerprint sha256:...]
                                   save server address and token
  warren tcp  LOCAL [--port N]     expose a TCP service (e.g. Minecraft Java)
  warren udp  LOCAL [--port N]     expose a UDP service (e.g. Minecraft Bedrock)
  warren http LOCAL [--subdomain NAME] [--auth USER:PASS] [--allow IPS]
                                   expose a website at NAME.yourdomain

  LOCAL is a port (25565) or host:port (192.168.1.5:25565).
  Run "warren tcp -h" for all flags.

Background (works with tcp, udp, http and server):
  warren tcp 25565 -b              run in the background; the terminal can be closed
  warren ps                        list warren processes running in the background
  warren stop PID|all              stop one (or all) of them

Server (config: --config FILE, $WARREN_CONFIG, or /etc/warren/server.json):
  warren server                    run the tunnel server
  warren adduser NAME              create a user and print their login command
  warren deluser NAME              remove a user
  warren users                     list users
  warren fingerprint               print the TLS fingerprint clients pin with --fingerprint

  warren update                    update warren to the latest release
  warren update --check            only check whether there's a newer version
  warren version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	removeOldExecutable()

	var err error
	if cmd == "tcp" || cmd == "udp" || cmd == "http" || cmd == "server" {
		if rest, bg := takeBackgroundFlag(args); bg {
			if err := startBackground(append([]string{cmd}, rest...)); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}
	switch cmd {
	case "tcp", "udp", "http":
		err = cmdTunnel(cmd, args)
	case "ps":
		err = cmdPs(args)
	case "stop":
		err = cmdStop(args)
	case "login":
		err = cmdLogin(args)
	case "server":
		err = cmdServer(args)
	case "adduser":
		err = cmdAddUser(args)
	case "deluser":
		err = cmdDelUser(args)
	case "users":
		err = cmdUsers(args)
	case "fingerprint":
		err = cmdFingerprint(args)
	case "update":
		err = cmdUpdate(args)
	case "version", "--version":
		fmt.Println("warren", version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
