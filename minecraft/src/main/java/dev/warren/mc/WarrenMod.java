package dev.warren.mc;

import java.nio.file.Path;
import java.util.UUID;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import com.mojang.brigadier.arguments.IntegerArgumentType;
import com.mojang.brigadier.context.CommandContext;

import net.fabricmc.api.ModInitializer;
import net.fabricmc.fabric.api.command.v2.CommandRegistrationCallback;
import net.fabricmc.fabric.api.event.lifecycle.v1.ServerLifecycleEvents;
import net.fabricmc.loader.api.FabricLoader;
import net.minecraft.ChatFormatting;
import net.minecraft.commands.CommandSourceStack;
import net.minecraft.commands.Commands;
import static net.minecraft.commands.Commands.argument;
import static net.minecraft.commands.Commands.literal;
import net.minecraft.network.chat.ClickEvent;
import net.minecraft.network.chat.Component;
import net.minecraft.network.chat.HoverEvent;
import net.minecraft.network.chat.MutableComponent;
import net.minecraft.server.MinecraftServer;
import net.minecraft.server.level.ServerPlayer;

/**
 * Adds /warren open|close|status. Everything runs on the (integrated) server,
 * so the command only uses vanilla argument types and players joining the
 * world don't need the mod.
 */
public class WarrenMod implements ModInitializer {
	static final Logger LOG = LoggerFactory.getLogger("warren");

	/**
	 * Opens the settings screen on this game's client. Set by WarrenClient, so
	 * it stays null on a dedicated server, where there is no screen to show.
	 */
	static volatile Runnable settingsScreenOpener;

	/** The tunnel for the running server, if any. Only touched on the server thread. */
	private static WarrenTunnel tunnel;

	@Override
	public void onInitialize() {
		WarrenConfig.writeTemplateIfMissing(configDir());

		CommandRegistrationCallback.EVENT.register((dispatcher, registryAccess, environment) ->
			dispatcher.register(literal("warren")
				.requires(WarrenMod::isHost)
				.then(literal("open")
					.executes(ctx -> open(ctx, -1))
					.then(argument("port", IntegerArgumentType.integer(1, 65535))
						.executes(ctx -> open(ctx, IntegerArgumentType.getInteger(ctx, "port")))))
				.then(literal("login").executes(WarrenMod::login))
				.then(literal("close").executes(WarrenMod::close))
				.then(literal("status").executes(WarrenMod::status))));

		ServerLifecycleEvents.SERVER_STOPPING.register(server -> {
			if (tunnel != null) {
				tunnel.close();
				tunnel = null;
			}
		});
	}

	private static Path configDir() {
		return FabricLoader.getInstance().getConfigDir();
	}

	/** In singleplayer only the world's owner may use /warren; on a dedicated server, owners/console. */
	private static boolean isHost(CommandSourceStack src) {
		MinecraftServer server = src.getServer();
		// The command tree is also checked against a dummy source with no server.
		if (server == null || server.isDedicatedServer()) {
			return Commands.LEVEL_OWNERS.check(src.permissions());
		}
		ServerPlayer player = src.getPlayer();
		return player != null && server.isSingleplayerOwner(player.nameAndId());
	}

	private static int open(CommandContext<CommandSourceStack> ctx, int portArg) {
		CommandSourceStack src = ctx.getSource();
		MinecraftServer server = src.getServer();

		if (tunnel != null && !tunnel.isClosed()) {
			String addr = tunnel.address();
			src.sendFailure(addr != null
				? Component.literal("warren is already open at " + addr + ". Use /warren close first.")
				: Component.literal("warren is already connecting. Use /warren close to cancel."));
			return 0;
		}
		WarrenConfig cfg;
		try {
			cfg = WarrenConfig.load(configDir());
		} catch (Exception e) {
			src.sendFailure(Component.literal("Could not read warren settings: " + e.getMessage()));
			return 0;
		}
		String problem = cfg.validate();
		if (problem != null && openSettingsScreen(server)) {
			src.sendFailure(Component.literal(problem + ". Enter your login, save, then run /warren open again."));
			return 0;
		}
		if (problem != null) {
			Path file = WarrenConfig.modConfigPath(configDir()).toAbsolutePath();
			src.sendFailure(Component.literal(problem + ". Put your server and token in ")
				.append(Component.literal(file.toString()).withStyle(s -> s
					.withUnderlined(true)
					.withClickEvent(new ClickEvent.CopyToClipboard(file.toString()))
					.withHoverEvent(new HoverEvent.ShowText(Component.literal("Click to copy")))))
				.append(" (or run \"warren login\" on this PC), then try again."));
			return 0;
		}
		if (!server.isPublished()) {
			src.sendFailure(Component.literal("Open your world to LAN first (Esc → Open to LAN), then run /warren open."));
			return 0;
		}

		String localIp = server.getLocalIp();
		String localHost = localIp == null || localIp.isEmpty() ? "127.0.0.1" : localIp;
		int localPort = server.getPort();
		int publicPort = portArg > 0 ? portArg : cfg.port;
		UUID who = src.getPlayer() == null ? null : src.getPlayer().getUUID();

		WarrenTunnel t = new WarrenTunnel(cfg, publicPort, localHost, localPort, new WarrenTunnel.Listener() {
			private boolean announcedDrop;

			@Override
			public void online(String address, boolean reconnected) {
				announcedDrop = false;
				LOG.info("{} at {} -> {}:{}", reconnected ? "reconnected" : "online", address, localHost, localPort);
				tell(server, who, reconnected
					? prefix().append(Component.literal("Reconnected. Your world is back online at ")).append(address(address))
					: prefix().append(Component.literal("Your world is online! ")).append(address(address))
						.append(Component.literal("")));
			}

			@Override
			public void reconnecting(String reason, long delayMs) {
				LOG.warn("disconnected ({}), reconnecting in {}s", reason, delayMs / 1000);
				if (!announcedDrop) {
					announcedDrop = true;
					tell(server, who, prefix().append(Component.literal("Lost connection to warren (" + reason + "), reconnecting…")
						.withStyle(ChatFormatting.YELLOW)));
				}
			}

			@Override
			public void failed(String reason) {
				LOG.warn("stopped: {}", reason);
				tell(server, who, prefix().append(Component.literal("Could not open your world: " + reason)
					.withStyle(ChatFormatting.RED)));
			}
		});
		tunnel = t;
		t.start();
		src.sendSuccess(() -> prefix().append(Component.literal("Connecting to " + cfg.server + "…")), false);
		return 1;
	}

	private static int login(CommandContext<CommandSourceStack> ctx) {
		CommandSourceStack src = ctx.getSource();
		if (!openSettingsScreen(src.getServer())) {
			Path file = WarrenConfig.modConfigPath(configDir()).toAbsolutePath();
			src.sendFailure(Component.literal("On a dedicated server, put your server and token in " + file + " instead."));
			return 0;
		}
		return 1;
	}

	/** Shows the settings screen to the host, if this server runs inside a game client. */
	private static boolean openSettingsScreen(MinecraftServer server) {
		Runnable opener = settingsScreenOpener;
		if (server.isDedicatedServer() || opener == null) {
			return false;
		}
		opener.run();
		return true;
	}

	private static int close(CommandContext<CommandSourceStack> ctx) {
		CommandSourceStack src = ctx.getSource();
		if (tunnel == null || tunnel.isClosed()) {
			src.sendFailure(Component.literal("warren is not open."));
			return 0;
		}
		tunnel.close();
		tunnel = null;
		src.sendSuccess(() -> prefix().append(Component.literal("Closed. Your world is only reachable on your LAN again.")), false);
		return 1;
	}

	private static int status(CommandContext<CommandSourceStack> ctx) {
		CommandSourceStack src = ctx.getSource();
		if (tunnel == null || tunnel.isClosed()) {
			src.sendSuccess(() -> prefix().append(Component.literal("Not open. Use /warren open.")), false);
		} else if (tunnel.address() == null) {
			src.sendSuccess(() -> prefix().append(Component.literal("Connecting…")), false);
		} else {
			String addr = tunnel.address();
			src.sendSuccess(() -> prefix().append(Component.literal("Online at ")).append(address(addr)), false);
		}
		return 1;
	}

	private static MutableComponent prefix() {
		return Component.literal("[warren] ").withStyle(ChatFormatting.GOLD);
	}

	private static MutableComponent address(String addr) {
		return Component.literal(addr).withStyle(s -> s
			.withColor(ChatFormatting.AQUA)
			.withUnderlined(true)
			.withClickEvent(new ClickEvent.CopyToClipboard(addr))
			.withHoverEvent(new HoverEvent.ShowText(Component.literal("Click to copy"))));
	}

	/** Sends a message from a background thread to the player who opened the tunnel. */
	private static void tell(MinecraftServer server, UUID who, Component msg) {
		server.execute(() -> {
			ServerPlayer p = who == null ? null : server.getPlayerList().getPlayer(who);
			if (p != null) {
				p.sendSystemMessage(msg);
			} else if (who == null) {
				server.sendSystemMessage(msg);
			}
		});
	}
}
