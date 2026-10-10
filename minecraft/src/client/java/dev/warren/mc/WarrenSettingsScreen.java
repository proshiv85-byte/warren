package dev.warren.mc;

import java.io.IOException;
import java.nio.file.Path;
import net.fabricmc.loader.api.FabricLoader;
import net.minecraft.ChatFormatting;
import net.minecraft.client.gui.GuiGraphics;
import net.minecraft.client.gui.components.Button;
import net.minecraft.client.gui.components.Checkbox;
import net.minecraft.client.gui.components.EditBox;
import net.minecraft.client.gui.components.Tooltip;
import net.minecraft.client.gui.screens.Screen;
import net.minecraft.network.chat.Component;
import net.minecraft.network.chat.FormattedText;
import net.minecraft.network.chat.Style;
import net.minecraft.util.FormattedCharSequence;

/** Edits config/warren.json: server address, token, TLS and the public port. */
final class WarrenSettingsScreen extends Screen {
	private static final int WHITE = 0xFFFFFFFF;
	private static final int GRAY = 0xFFA0A0A0;
	private static final int RED = 0xFFFF5555;

	private final Screen parent;
	private final WarrenConfig initial;
	private String error;

	private EditBox serverBox;
	private EditBox tokenBox;
	private EditBox portBox;
	private Checkbox tlsBox;
	private Checkbox insecureBox;
	private boolean applyingPaste;
	private String lastServer = "";
	private String lastToken = "";

	WarrenSettingsScreen(Screen parent) {
		super(Component.literal("warren settings"));
		this.parent = parent;
		WarrenConfig cfg;
		try {
			cfg = WarrenConfig.load(configDir());
		} catch (IOException e) {
			cfg = new WarrenConfig();
			error = e.getMessage();
		}
		this.initial = cfg;
	}

	private static Path configDir() {
		return FabricLoader.getInstance().getConfigDir();
	}

	private int left() {
		return width / 2 - 100;
	}

	private int top() {
		return Math.max(30, height / 2 - 100);
	}

	@Override
	protected void init() {
		// Keep what was typed when the window is resized.
		String server = serverBox != null ? serverBox.getValue() : initial.server;
		String token = tokenBox != null ? tokenBox.getValue() : initial.token;
		String port = portBox != null ? portBox.getValue() : initial.port > 0 ? Integer.toString(initial.port) : "";
		boolean tls = tlsBox != null ? tlsBox.selected() : initial.tls;
		boolean insecure = insecureBox != null ? insecureBox.selected() : initial.insecure;

		int x = left();
		int y = top();

		serverBox = new EditBox(font, x, y + 10, 200, 20, Component.literal("Server"));
		serverBox.setMaxLength(1024);
		serverBox.setValue(server);
		serverBox.setHint(Component.literal("tunnel.example.com:7000").withStyle(ChatFormatting.DARK_GRAY));
		lastServer = server;
		serverBox.setResponder(text -> {
			String before = lastServer;
			lastServer = text;
			maybePasteLogin(before, text);
		});
		addRenderableWidget(serverBox);

		tokenBox = new EditBox(font, x, y + 46, 200, 20, Component.literal("Token"));
		tokenBox.setMaxLength(1024);
		tokenBox.setValue(token);
		tokenBox.setHint(Component.literal("your token").withStyle(ChatFormatting.DARK_GRAY));
		// Hide the token unless you're typing in it (it's as good as a password).
		tokenBox.addFormatter((text, offset) -> tokenBox.isFocused()
			? FormattedCharSequence.forward(text, Style.EMPTY)
			: FormattedCharSequence.forward("•".repeat(text.length()), Style.EMPTY));
		lastToken = token;
		tokenBox.setResponder(text -> {
			String before = lastToken;
			lastToken = text;
			maybePasteLogin(before, text);
		});
		addRenderableWidget(tokenBox);

		portBox = new EditBox(font, x, y + 82, 60, 20, Component.literal("Public port"));
		portBox.setMaxLength(5);
		portBox.setFilter(s -> s.chars().allMatch(Character::isDigit));
		portBox.setValue(port);
		portBox.setHint(Component.literal("any").withStyle(ChatFormatting.DARK_GRAY));
		addRenderableWidget(portBox);

		tlsBox = addRenderableWidget(Checkbox.builder(Component.literal("Use TLS"), font)
			.pos(x, y + 110)
			.selected(tls)
			.tooltip(Tooltip.create(Component.literal("Turn on if your login command ends with --tls")))
			.build());
		insecureBox = addRenderableWidget(Checkbox.builder(Component.literal("Skip certificate check"), font)
			.pos(x, y + 132)
			.selected(insecure)
			.tooltip(Tooltip.create(Component.literal("Only for servers with a self-signed certificate (--insecure)")))
			.build());

		addRenderableWidget(Button.builder(Component.literal("Save"), b -> save())
			.bounds(x, y + 180, 98, 20).build());
		addRenderableWidget(Button.builder(Component.literal("Cancel"), b -> onClose())
			.bounds(x + 102, y + 180, 98, 20).build());

		setInitialFocus(server.isEmpty() ? serverBox : tokenBox);
	}

	/**
	 * If a whole "warren login --server … --token … --tls" command is pasted
	 * into either box, fill in every field from it.
	 */
	private void maybePasteLogin(String before, String text) {
		// Only react to a paste (many characters at once), not to typing.
		boolean pasted = text.length() - before.length() > 1;
		if (applyingPaste || !pasted || !(text.contains("--token") || text.contains("--server"))) {
			return;
		}
		String server = null, token = null;
		boolean tls = false, insecure = false;
		String[] words = text.trim().split("\\s+");
		for (int i = 0; i < words.length; i++) {
			String w = words[i].replaceFirst("^--?", "--");
			String value = null;
			int eq = w.indexOf('=');
			if (eq > 0) {
				value = w.substring(eq + 1);
				w = w.substring(0, eq);
			} else if ((w.equals("--server") || w.equals("--token")) && i + 1 < words.length) {
				value = words[++i];
			}
			switch (w) {
				case "--server" -> server = value;
				case "--token" -> token = value;
				case "--tls" -> tls = value == null || value.equals("true");
				case "--insecure" -> insecure = value == null || value.equals("true");
				default -> {
				}
			}
		}
		applyingPaste = true;
		serverBox.setValue(server != null ? server : serverBox == getFocused() ? "" : serverBox.getValue());
		tokenBox.setValue(token != null ? token : tokenBox == getFocused() ? "" : tokenBox.getValue());
		lastServer = serverBox.getValue();
		lastToken = tokenBox.getValue();
		applyingPaste = false;
		if (tls != tlsBox.selected()) {
			tlsBox.onPress(null);
		}
		if (insecure != insecureBox.selected()) {
			insecureBox.onPress(null);
		}
		error = null;
	}

	private void save() {
		WarrenConfig cfg = new WarrenConfig();
		cfg.server = serverBox.getValue().trim();
		cfg.token = tokenBox.getValue().trim();
		cfg.tls = tlsBox.selected();
		cfg.insecure = insecureBox.selected();
		cfg.port = portBox.getValue().isEmpty() ? 0 : Integer.parseInt(portBox.getValue());

		error = cfg.server.isEmpty() || cfg.token.isEmpty()
			? "Enter both a server address and a token."
			: cfg.validate();
		if (error != null) {
			return;
		}
		try {
			cfg.save(configDir());
		} catch (IOException e) {
			error = "Could not save: " + e.getMessage();
			return;
		}
		if (minecraft.player != null && parent == null) {
			minecraft.player.displayClientMessage(Component.literal("[warren] ").withStyle(ChatFormatting.GOLD)
				.append(Component.literal("Settings saved. Open to LAN, then run /warren open.").withStyle(ChatFormatting.WHITE)), false);
		}
		onClose();
	}

	@Override
	public void render(GuiGraphics g, int mouseX, int mouseY, float delta) {
		super.render(g, mouseX, mouseY, delta);
		int x = left();
		int y = top();
		g.drawCenteredString(font, title, width / 2, y - 20, WHITE);
		g.drawString(font, "Server address", x, y, GRAY);
		g.drawString(font, "Token", x, y + 36, GRAY);
		g.drawString(font, "Public port (empty = any)", x, y + 72, GRAY);
		if (error != null) {
			g.drawWordWrap(font, FormattedText.of(error), x, y + 156, 200, RED);
		} else {
			g.drawString(font, "Tip: paste your whole \"warren login\" command", x, y + 156, GRAY);
			g.drawString(font, "into the Server box to fill in everything.", x, y + 166, GRAY);
		}
	}

	@Override
	public void onClose() {
		minecraft.setScreen(parent);
	}
}
