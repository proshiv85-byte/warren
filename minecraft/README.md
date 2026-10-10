# warren for Minecraft (Fabric 1.21.11)

Share your singleplayer world with friends anywhere through your warren
server. Only the host needs the mod. Friends join with plain vanilla
Minecraft 1.21.11.

## Install

1. Install [Fabric Loader](https://fabricmc.net/use/) for 1.21.11 and put
   [Fabric API](https://modrinth.com/mod/fabric-api) and the warren mod
   (`warren-mod-…+mc1.21.11.jar` from the
   [releases page](https://github.com/proshiv85-byte/warren/releases))
   into your `mods` folder.
2. Log in once. In a world, type `/warren login` (or press **Esc → Open to
   LAN → warren…**) and fill in the settings window:
   - paste your whole `warren login --server … --token … --tls` command into
     the **Server address** box, which fills in everything at once, **or**
   - type the server address and token, and tick **Use TLS** if your login
     command has `--tls`. If it also has `--fingerprint` (servers with a
     self-signed certificate), paste the whole command instead so the
     fingerprint is saved too, or add `"fingerprint": "sha256:…"` to
     `warren.json`.
   - **Public port**: the port friends connect to. Leave it empty for any free port.

   The settings are saved in `.minecraft/config/warren.json`. If you already ran
   `warren login` on this PC, the mod picks that up automatically.

## Use

1. Open your world, press **Esc → Open to LAN → Start LAN World**.
2. Type `/warren open` in chat. You'll see something like
   `[warren] Your world is online! Friends can join at tunnel.example.com:25565`.
   Click the address to copy it.
3. Friends add that address as a server in Multiplayer.

| Command | What it does |
|---|---|
| `/warren login` | Open the settings window (server, token, TLS, port) |
| `/warren open` | Make your LAN world reachable through warren |
| `/warren open <port>` | Same, but ask for a specific public port |
| `/warren status` | Show the public address |
| `/warren close` | Stop sharing; players who joined through warren are disconnected |

The tunnel also closes on its own when you leave the world. Only the world's
owner can use `/warren`, and cheats don't need to be on. If your internet drops,
the mod reconnects by itself and keeps the same port.

The mod also works on a dedicated Fabric server, where `/warren` is limited
to the console and level-4 operators.

## Building

Requires JDK 25+ to run Gradle (Fabric Loom needs it); the mod itself targets Java 21.

```sh
./gradlew build    # build/libs/warren-0.1.0.jar
```
