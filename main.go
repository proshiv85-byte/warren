// warren is a small self-hosted tunnel server and client: expose local
// TCP, UDP and HTTP services through a VPS you control.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0"

const usageText = `warren - self-hosted tunnels (TCP, UDP, HTTP)

Client:
  warren login --server HOST[:PORT] --token TOKEN [--tls]
                                   save server address and token
  warren tcp  LOCAL [--port N]     expose a TCP service (e.g. Minecraft Java)
  warren udp  LOCAL [--port N]     expose a UDP service (e.g. Minecraft Bedrock)
  warren http LOCAL [--subdomain NAME]
                                   expose a website at NAME.yourdomain

  LOCAL is a port (25565) or host:port (192.168.1.5:25565).
  Run "warren tcp -h" for all flags.

Server (config: --config FILE, $WARREN_CONFIG, or /etc/warren/server.json):
  warren server                    run the tunnel server
  warren adduser NAME              create a user and print their login command
  warren deluser NAME              remove a user
  warren users                     list users

  warren version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "tcp", "udp", "http":
		err = cmdTunnel(cmd, args)
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
