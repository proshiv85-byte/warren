package dev.warren.mc;

import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import java.io.BufferedReader;
import java.io.EOFException;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.security.cert.CertificateException;
import java.security.cert.X509Certificate;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLParameters;
import javax.net.ssl.SSLSocket;
import javax.net.ssl.SSLSocketFactory;
import javax.net.ssl.TrustManager;
import javax.net.ssl.X509TrustManager;

/**
 * A TCP tunnel client speaking the warren wire protocol (see protocol.go):
 * a control connection announces each visitor, and the client answers with
 * a data connection that it pipes to the local Minecraft server.
 */
final class WarrenTunnel {
	private static final int PROTO_VERSION = 1;
	private static final int DIAL_TIMEOUT_MS = 10_000;
	private static final int HANDSHAKE_TIMEOUT_MS = 15_000;
	/** Server pings every 20s; give up on the connection after 60s of silence. */
	private static final int CONTROL_TIMEOUT_MS = 60_000;
	private static final int LOCAL_DIAL_TIMEOUT_MS = 5_000;

	interface Listener {
		/** The tunnel is reachable at address (host:port). */
		void online(String address, boolean reconnected);

		/** The connection dropped and will be retried after delayMs. */
		void reconnecting(String reason, long delayMs);

		/** The tunnel stopped for good (not called after close()). */
		void failed(String reason);
	}

	private static final class FatalException extends IOException {
		FatalException(String msg) {
			super(msg);
		}
	}

	private final WarrenConfig cfg;
	private final String localHost;
	private final int localPort;
	private final Listener listener;
	private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();
	private final Thread thread;
	private volatile boolean closed;
	private volatile String address;
	private int port;
	private boolean everOnline;

	WarrenTunnel(WarrenConfig cfg, int port, String localHost, int localPort, Listener listener) {
		this.cfg = cfg;
		this.port = port;
		this.localHost = localHost;
		this.localPort = localPort;
		this.listener = listener;
		this.thread = new Thread(this::run, "warren-control");
		this.thread.setDaemon(true);
	}

	void start() {
		thread.start();
	}

	/** The public address while online, otherwise null. */
	String address() {
		return address;
	}

	/** Stops the tunnel and disconnects everyone who joined through it. */
	void close() {
		closed = true;
		thread.interrupt();
		for (Socket s : sockets) {
			closeQuietly(s);
		}
	}

	boolean isClosed() {
		return closed;
	}

	private void run() {
		long backoff = 1000;
		while (!closed) {
			long start = System.currentTimeMillis();
			String reason;
			try {
				session();
				reason = "connection closed";
			} catch (FatalException e) {
				fail(e.getMessage());
				return;
			} catch (IOException e) {
				reason = describe(e);
			}
			address = null;
			if (closed) {
				return;
			}
			if (!everOnline) {
				// Nothing has worked yet, so the settings are probably wrong;
				// report it now rather than retrying in the background forever.
				fail(reason);
				return;
			}
			if (System.currentTimeMillis() - start > 60_000) {
				backoff = 1000;
			}
			listener.reconnecting(reason, backoff);
			try {
				Thread.sleep(backoff);
			} catch (InterruptedException e) {
				return;
			}
			backoff = Math.min(backoff * 2, 30_000);
		}
	}

	private void fail(String reason) {
		address = null;
		if (!closed) {
			close();
			listener.failed(reason);
		}
	}

	private String describe(IOException e) {
		if (e instanceof EOFException && !everOnline && !cfg.tls) {
			return "the server closed the connection (if it uses TLS, set \"tls\": true)";
		}
		if (e instanceof SocketTimeoutException) {
			return "timed out";
		}
		String msg = e.getMessage();
		return msg == null || msg.isEmpty() ? e.getClass().getSimpleName() : msg;
	}

	private void session() throws IOException {
		Socket conn = dial();
		try {
			conn.setSoTimeout(HANDSHAKE_TIMEOUT_MS);
			OutputStream out = conn.getOutputStream();
			JsonObject hello = new JsonObject();
			hello.addProperty("type", "hello");
			hello.addProperty("version", PROTO_VERSION);
			hello.addProperty("token", cfg.token);
			hello.addProperty("proto", "tcp");
			if (port != 0) {
				hello.addProperty("port", port);
			}
			writeMsg(out, hello);

			BufferedReader in = new BufferedReader(new InputStreamReader(conn.getInputStream(), StandardCharsets.UTF_8));
			JsonObject m = readMsg(in);
			String type = str(m, "type");
			if (type.equals("error")) {
				String err = "server: " + str(m, "error");
				if (!everOnline || m.has("fatal") && m.get("fatal").getAsBoolean()) {
					// Port taken, bad token... retrying won't help.
					throw new FatalException(err);
				}
				throw new IOException(err);
			}
			if (!type.equals("ok")) {
				throw new IOException("unexpected reply \"" + type + "\"");
			}
			conn.setSoTimeout(CONTROL_TIMEOUT_MS);

			// Ask for the same port again if we have to reconnect.
			if (m.has("port")) {
				port = m.get("port").getAsInt();
			}
			String url = str(m, "url");
			address = url.startsWith("tcp://") ? url.substring("tcp://".length()) : url;
			boolean reconnected = everOnline;
			everOnline = true;
			listener.online(address, reconnected);

			while (!closed) {
				m = readMsg(in);
				switch (str(m, "type")) {
					case "ping" -> {
						JsonObject pong = new JsonObject();
						pong.addProperty("type", "pong");
						writeMsg(out, pong);
					}
					case "conn" -> {
						String id = str(m, "id");
						String remote = str(m, "remote");
						Thread t = new Thread(() -> handleStream(id, remote), "warren-stream");
						t.setDaemon(true);
						t.start();
					}
					case "error" -> {
						String err = "server: " + str(m, "error");
						if (m.has("fatal") && m.get("fatal").getAsBoolean()) {
							throw new FatalException(err);
						}
						throw new IOException(err);
					}
					default -> {
					}
				}
			}
		} finally {
			closeQuietly(conn);
		}
	}

	private void handleStream(String id, String remote) {
		Socket local = null;
		IOException localErr = null;
		try {
			local = track(new Socket());
			local.setTcpNoDelay(true);
			local.connect(new InetSocketAddress(localHost, localPort), LOCAL_DIAL_TIMEOUT_MS);
		} catch (IOException e) {
			closeQuietly(local);
			local = null;
			localErr = e;
		}

		Socket data;
		try {
			data = dial();
			data.setSoTimeout(0);
			JsonObject join = new JsonObject();
			join.addProperty("type", "join");
			join.addProperty("id", id);
			writeMsg(data.getOutputStream(), join);
		} catch (IOException e) {
			WarrenMod.LOG.warn("opening stream for {}: {}", remote, describe(e));
			closeQuietly(local);
			return;
		}

		if (local == null) {
			WarrenMod.LOG.warn("connection from {}, but the world is not reachable on {}:{}: {}", remote, localHost, localPort, localErr.toString());
			closeQuietly(data);
			return;
		}
		WarrenMod.LOG.info("connection from {}", remote);
		Socket l = local;
		Thread up = new Thread(() -> copy(data, l), "warren-pipe");
		up.setDaemon(true);
		up.start();
		copy(l, data);
	}

	/** Copies src to dst until either side ends, then closes both. */
	private void copy(Socket src, Socket dst) {
		byte[] buf = new byte[32 * 1024];
		try {
			InputStream in = src.getInputStream();
			OutputStream out = dst.getOutputStream();
			int n;
			while ((n = in.read(buf)) >= 0) {
				out.write(buf, 0, n);
			}
		} catch (IOException ignored) {
			// one side went away
		} finally {
			closeQuietly(src);
			closeQuietly(dst);
		}
	}

	private Socket dial() throws IOException {
		String host = cfg.host();
		int controlPort = cfg.controlPort();
		Socket raw = track(new Socket());
		try {
			raw.setKeepAlive(true);
			raw.setTcpNoDelay(true);
			raw.connect(new InetSocketAddress(host, controlPort), DIAL_TIMEOUT_MS);
			if (!cfg.tls) {
				return raw;
			}
			SSLSocket ssl = (SSLSocket) tlsFactory().createSocket(raw, host, controlPort, true);
			sockets.remove(raw); // closed together with ssl from now on
			track(ssl);
			if (!cfg.insecure && !pinned()) {
				SSLParameters params = ssl.getSSLParameters();
				params.setEndpointIdentificationAlgorithm("HTTPS");
				ssl.setSSLParameters(params);
			}
			ssl.setSoTimeout(HANDSHAKE_TIMEOUT_MS);
			ssl.startHandshake();
			return ssl;
		} catch (IOException e) {
			closeQuietly(raw);
			throw e;
		}
	}

	private boolean pinned() {
		return !cfg.fingerprint.isEmpty();
	}

	/**
	 * With a fingerprint, the only certificate trusted is the one whose
	 * public key matches it (self-signed certificates have no CA to check).
	 * With "insecure" and no fingerprint, any certificate is accepted.
	 */
	private SSLSocketFactory tlsFactory() throws IOException {
		if (!cfg.insecure && !pinned()) {
			return (SSLSocketFactory) SSLSocketFactory.getDefault();
		}
		byte[] pin = pinned() ? cfg.pinnedKeyHash() : null;
		if (pinned() && pin == null) {
			throw new FatalException("bad fingerprint \"" + cfg.fingerprint + "\"");
		}
		try {
			SSLContext ctx = SSLContext.getInstance("TLS");
			ctx.init(null, new TrustManager[] {new X509TrustManager() {
				@Override
				public void checkClientTrusted(X509Certificate[] chain, String authType) {}

				@Override
				public void checkServerTrusted(X509Certificate[] chain, String authType) throws CertificateException {
					if (pin == null) {
						return;
					}
					if (chain == null || chain.length == 0) {
						throw new CertificateException("the server sent no certificate");
					}
					byte[] got;
					try {
						got = MessageDigest.getInstance("SHA-256").digest(chain[0].getPublicKey().getEncoded());
					} catch (GeneralSecurityException e) {
						throw new CertificateException(e);
					}
					if (!MessageDigest.isEqual(got, pin)) {
						throw new CertificateException("the server's certificate doesn't match the fingerprint"
							+ " (if the server owner made a new certificate, ask them for the new one)");
					}
				}

				@Override
				public X509Certificate[] getAcceptedIssuers() {
					return new X509Certificate[0];
				}
			}}, null);
			return ctx.getSocketFactory();
		} catch (GeneralSecurityException e) {
			throw new IOException(e);
		}
	}

	/** Remembers s so close() can shut it; forgotten again once closed. */
	private Socket track(Socket s) throws IOException {
		sockets.add(s);
		if (closed) {
			closeQuietly(s);
			throw new IOException("tunnel closed");
		}
		return s;
	}

	private void closeQuietly(Socket s) {
		if (s == null) {
			return;
		}
		sockets.remove(s);
		try {
			s.close();
		} catch (IOException ignored) {
		}
	}

	private static void writeMsg(OutputStream out, JsonObject m) throws IOException {
		synchronized (out) {
			out.write((m + "\n").getBytes(StandardCharsets.UTF_8));
			out.flush();
		}
	}

	private static JsonObject readMsg(BufferedReader in) throws IOException {
		String line = in.readLine();
		if (line == null) {
			throw new EOFException("EOF");
		}
		try {
			return JsonParser.parseString(line).getAsJsonObject();
		} catch (RuntimeException e) {
			throw new IOException("bad message from server");
		}
	}

	private static String str(JsonObject m, String key) {
		return m.has(key) && !m.get(key).isJsonNull() ? m.get(key).getAsString() : "";
	}
}
