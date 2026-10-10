package dev.warren.mc;

import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.JsonParseException;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.PosixFilePermissions;

/**
 * Where to find the warren server. Read from config/warren.json first; any
 * field left empty there falls back to the settings saved by
 * "warren login" (the same client.json the Go client uses), then to the
 * WARREN_SERVER / WARREN_TOKEN environment variables.
 */
final class WarrenConfig {
	static final String DEFAULT_CONTROL_PORT = "7000";
	private static final Gson GSON = new GsonBuilder().setPrettyPrinting().create();

	String server = "";
	String token = "";
	boolean tls;
	boolean insecure;
	/** Public port to ask the server for; 0 means any free port. */
	int port;

	static Path modConfigPath(Path configDir) {
		return configDir.resolve("warren.json");
	}

	/** Writes an empty config file so players know where to put their settings. */
	static void writeTemplateIfMissing(Path configDir) {
		Path p = modConfigPath(configDir);
		if (Files.exists(p)) {
			return;
		}
		try {
			Files.createDirectories(configDir);
			Files.writeString(p, GSON.toJson(new WarrenConfig()) + "\n");
		} catch (IOException e) {
			WarrenMod.LOG.warn("could not create {}: {}", p, e.toString());
		}
	}

	/** Saves to config/warren.json, readable only by this user where the OS allows. */
	void save(Path configDir) throws IOException {
		Path p = modConfigPath(configDir);
		Files.createDirectories(configDir);
		Files.writeString(p, GSON.toJson(this) + "\n");
		try {
			Files.setPosixFilePermissions(p, PosixFilePermissions.fromString("rw-------"));
		} catch (UnsupportedOperationException ignored) {
			// Windows: the file stays in the user's own profile anyway
		}
	}

	static WarrenConfig load(Path configDir) throws IOException {
		WarrenConfig cfg = read(modConfigPath(configDir));
		if (cfg == null) {
			cfg = new WarrenConfig();
		}
		if (cfg.server.isBlank() || cfg.token.isBlank()) {
			WarrenConfig login = read(goClientConfigPath());
			if (login != null) {
				if (cfg.server.isBlank() && cfg.token.isBlank()) {
					cfg.tls = login.tls;
					cfg.insecure = login.insecure;
				}
				if (cfg.server.isBlank()) cfg.server = login.server;
				if (cfg.token.isBlank()) cfg.token = login.token;
			}
		}
		String env = System.getenv("WARREN_SERVER");
		if (cfg.server.isBlank() && env != null) cfg.server = env;
		env = System.getenv("WARREN_TOKEN");
		if (cfg.token.isBlank() && env != null) cfg.token = env;
		cfg.server = cfg.server.trim();
		cfg.token = cfg.token.trim();
		return cfg;
	}

	private static WarrenConfig read(Path p) throws IOException {
		if (p == null || !Files.isRegularFile(p)) {
			return null;
		}
		try {
			WarrenConfig cfg = GSON.fromJson(Files.readString(p), WarrenConfig.class);
			if (cfg != null) {
				if (cfg.server == null) cfg.server = "";
				if (cfg.token == null) cfg.token = "";
			}
			return cfg;
		} catch (JsonParseException e) {
			throw new IOException(p + " is not valid JSON: " + e.getMessage());
		}
	}

	/** Mirrors Go's os.UserConfigDir()/warren/client.json. */
	private static Path goClientConfigPath() {
		String os = System.getProperty("os.name", "").toLowerCase();
		String home = System.getProperty("user.home");
		String base;
		if (os.contains("win")) {
			base = System.getenv("AppData");
		} else if (os.contains("mac")) {
			base = home == null ? null : home + "/Library/Application Support";
		} else {
			base = System.getenv("XDG_CONFIG_HOME");
			if (base == null || base.isEmpty()) {
				base = home == null ? null : home + "/.config";
			}
		}
		return base == null ? null : Path.of(base, "warren", "client.json");
	}

	/** Splits "host[:port]" (IPv6 as "[::1]:7000"), defaulting the control port. */
	String host() {
		return splitServer()[0];
	}

	int controlPort() {
		return Integer.parseInt(splitServer()[1]);
	}

	private String[] splitServer() {
		String s = server;
		if (s.startsWith("[")) {
			int end = s.indexOf(']');
			String host = s.substring(1, end);
			String port = s.startsWith(":", end + 1) ? s.substring(end + 2) : DEFAULT_CONTROL_PORT;
			return new String[] {host, port};
		}
		int colon = s.indexOf(':');
		if (colon >= 0 && colon == s.lastIndexOf(':')) {
			return new String[] {s.substring(0, colon), s.substring(colon + 1)};
		}
		return new String[] {s, DEFAULT_CONTROL_PORT};
	}

	/** Returns a problem with the settings, or null if they look usable. */
	String validate() {
		if (server.isEmpty() || token.isEmpty()) {
			return "no warren server/token set";
		}
		if (server.startsWith("[") && server.indexOf(']') < 0) {
			return "bad server address \"" + server + "\"";
		}
		try {
			int p = controlPort();
			if (p < 1 || p > 65535) throw new NumberFormatException();
		} catch (NumberFormatException e) {
			return "bad server address \"" + server + "\" (expected host or host:port)";
		}
		if (port < 0 || port > 65535) {
			return "port in warren.json must be between 0 and 65535";
		}
		return null;
	}
}
