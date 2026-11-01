package flex.android;

import org.json.JSONObject;

import java.net.DatagramPacket;
import java.net.DatagramSocket;
import java.net.InetAddress;
import java.net.InterfaceAddress;
import java.net.NetworkInterface;
import java.net.SocketTimeoutException;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Finds FLEX servers the way the PS5 does: a UDP broadcast every server
 *  answers with its name, id, port and (when signed in) Tailscale address. */
final class Discovery {
    static final int PORT = 7555;
    static final String MAGIC = "FLEX_DISCOVER";

    static final class Found {
        String name, id, ip, tsIp, version;
        int port;

        String url() { return "http://" + ip + ":" + port; }

        /** The address that works away from home, or null. */
        String tsUrl() { return tsIp == null || tsIp.isEmpty() ? null : "http://" + tsIp + ":" + port; }

        @Override public String toString() { return name + "  (" + ip + ")"; }
    }

    /** Broadcasts and collects answers for `millis`. Call off the main thread. */
    static List<Found> run(int millis) {
        Map<String, Found> found = new LinkedHashMap<>();
        try (DatagramSocket sock = new DatagramSocket()) {
            sock.setBroadcast(true);
            sock.setSoTimeout(250);
            byte[] msg = MAGIC.getBytes("UTF-8");
            for (InetAddress to : broadcastAddresses()) {
                try {
                    sock.send(new DatagramPacket(msg, msg.length, to, PORT));
                } catch (Exception ignored) {
                }
            }
            long end = System.currentTimeMillis() + millis;
            byte[] buf = new byte[1024];
            while (System.currentTimeMillis() < end) {
                DatagramPacket p = new DatagramPacket(buf, buf.length);
                try {
                    sock.receive(p);
                } catch (SocketTimeoutException e) {
                    continue;
                }
                try {
                    JSONObject j = new JSONObject(new String(p.getData(), 0, p.getLength(), "UTF-8"));
                    Found f = new Found();
                    f.ip = p.getAddress().getHostAddress();
                    f.name = j.optString("name", f.ip);
                    f.id = j.optString("id", "");
                    f.port = j.optInt("port", 8484);
                    f.tsIp = j.optString("ts_ip", "");
                    f.version = j.optString("version", "");
                    found.put(f.id.isEmpty() ? f.ip : f.id, f);
                } catch (Exception ignored) {
                }
            }
        } catch (Exception ignored) {
        }
        return new ArrayList<>(found.values());
    }

    private static List<InetAddress> broadcastAddresses() {
        List<InetAddress> out = new ArrayList<>();
        try {
            out.add(InetAddress.getByName("255.255.255.255"));
        } catch (Exception ignored) {
        }
        try {
            for (NetworkInterface ni : Collections.list(NetworkInterface.getNetworkInterfaces())) {
                if (!ni.isUp() || ni.isLoopback()) continue;
                for (InterfaceAddress ia : ni.getInterfaceAddresses()) {
                    if (ia.getBroadcast() != null) out.add(ia.getBroadcast());
                }
            }
        } catch (Exception ignored) {
        }
        return out;
    }
}
