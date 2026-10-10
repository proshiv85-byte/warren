package dev.warren.mc;

import net.fabricmc.api.ClientModInitializer;
import net.fabricmc.fabric.api.client.screen.v1.ScreenEvents;
import net.fabricmc.fabric.api.client.screen.v1.Screens;
import net.minecraft.client.Minecraft;
import net.minecraft.client.gui.components.Button;
import net.minecraft.client.gui.components.Tooltip;
import net.minecraft.client.gui.screens.ShareToLanScreen;
import net.minecraft.network.chat.Component;

/** Client side of the mod: the settings screen and the button that opens it. */
public class WarrenClient implements ClientModInitializer {
	@Override
	public void onInitializeClient() {
		// /warren login runs on the integrated server; hop over to the client to show the screen.
		WarrenMod.settingsScreenOpener = () -> {
			Minecraft mc = Minecraft.getInstance();
			mc.execute(() -> mc.setScreen(new WarrenSettingsScreen(null)));
		};

		// A "warren…" button in the corner of the Open to LAN screen.
		ScreenEvents.AFTER_INIT.register((client, screen, width, height) -> {
			if (screen instanceof ShareToLanScreen) {
				Screens.getButtons(screen).add(Button.builder(Component.literal("warren…"),
						b -> client.setScreen(new WarrenSettingsScreen(screen)))
					.bounds(6, 6, 70, 20)
					.tooltip(Tooltip.create(Component.literal("Set up your warren server so you can share this world with /warren open")))
					.build());
			}
		});
	}
}
