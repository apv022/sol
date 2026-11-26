package app.sol;

import java.util.HashMap;
import java.util.Iterator;
import java.util.List;
import java.util.Map;

/**
 * The timing and bookkeeping of Wi-Fi Direct (WifiDirect), with no Android types so it is
 * tested on a plain JDK (android/test). Every decision about keys, tags and roles is the node's
 * (`libsol.so nearby match`); this class only says when to ask it again and when to let go.
 */
final class WifiDirectPolicy {
    /** A service record older than this no longer counts (the phone left, or its ad rotated). */
    static final long RECORD_TTL_MS = 5 * 60_000;
    /** The group owner removes a group nobody has been in for this long. */
    static final long IDLE_MS = 2 * 60_000;
    /** Service discovery is asked for again this often (Android ends one after about 2 minutes). */
    static final long DISCOVER_EVERY_MS = 2 * 60_000;
    /** A connect or create that has not formed a group by then is given up. */
    static final long PENDING_MS = 30_000;
    /** At most this many records are handed to the node. */
    static final int MAX_RECORDS = 64;

    private WifiDirectPolicy() { }

    /** One TXT record seen from a peer, by its P2P device address. */
    static final class Record {
        final Map<String, String> txt;
        final long at;
        Record(Map<String, String> txt, long at) { this.txt = new HashMap<>(txt); this.at = at; }
    }

    /**
     * Records a TXT record; returns whether it is news (a new peer or different content), i.e.
     * whether the node should be asked again. Only Sol's service type counts.
     */
    static boolean put(Map<String, Record> seen, String id, String fullDomain, Map<String, String> txt, long now) {
        if (id == null || id.isEmpty() || txt == null || fullDomain == null || !fullDomain.contains("_sol._udp")) return false;
        Record old = seen.get(id);
        seen.put(id, new Record(txt, now));
        return old == null || !old.txt.equals(txt) || now - old.at >= RECORD_TTL_MS;
    }

    /** Drops records older than RECORD_TTL_MS and caps the rest; returns how many remain. */
    static int prune(Map<String, Record> seen, long now) {
        for (Iterator<Map.Entry<String, Record>> it = seen.entrySet().iterator(); it.hasNext(); ) {
            if (now - it.next().getValue().at >= RECORD_TTL_MS) it.remove();
        }
        while (seen.size() > MAX_RECORDS) {
            String oldest = null;
            long at = Long.MAX_VALUE;
            for (Map.Entry<String, Record> e : seen.entrySet()) {
                if (e.getValue().at < at) { at = e.getValue().at; oldest = e.getKey(); }
            }
            seen.remove(oldest);
        }
        return seen.size();
    }

    /**
     * Whether a group this device owns should be removed: nobody has been in it for IDLE_MS.
     * lastMemberAt is when a client was last in it (or when it was formed).
     */
    static boolean ownerIdle(int clients, long lastMemberAt, long now) {
        return clients == 0 && now - lastMemberAt >= IDLE_MS;
    }

    /**
     * The IPv4 address beacons should use on the group's interface, from its addresses as
     * strings (the first private IPv4 one; Android's groups are 192.168.49.0/24), or null.
     */
    static String pickIpv4(List<String> addrs) {
        String any = null;
        for (String a : addrs) {
            if (a == null) continue;
            if (a.startsWith("/")) a = a.substring(1);
            String[] p = a.split("\\.");
            if (p.length != 4) continue;
            int[] o = new int[4];
            try {
                for (int i = 0; i < 4; i++) { o[i] = Integer.parseInt(p[i]); if (o[i] < 0 || o[i] > 255) throw new NumberFormatException(); }
            } catch (NumberFormatException e) { continue; }
            if (o[0] == 127 || o[0] == 0 || o[0] >= 224 || (o[0] == 169 && o[1] == 254)) continue;
            if (o[0] == 192 && o[1] == 168 && o[2] == 49) return a;
            if (any == null) any = a;
        }
        return any;
    }

    /** The JSON string literal of s (for the request the node reads on stdin). */
    static String quote(String s) {
        StringBuilder b = new StringBuilder("\"");
        for (char c : s.toCharArray()) {
            switch (c) {
                case '"': b.append("\\\""); break;
                case '\\': b.append("\\\\"); break;
                default:
                    if (c < 0x20) b.append(String.format("\\u%04x", (int) c));
                    else b.append(c);
            }
        }
        return b.append('"').toString();
    }

    /** The `nearby match` request: {"state":..., "seen":[{"id":..., "txt":{...}}]}. */
    static String matchRequest(String state, Map<String, Record> seen) {
        StringBuilder b = new StringBuilder("{\"state\":").append(quote(state)).append(",\"seen\":[");
        boolean first = true;
        for (Map.Entry<String, Record> e : seen.entrySet()) {
            if (!first) b.append(',');
            first = false;
            b.append("{\"id\":").append(quote(e.getKey())).append(",\"txt\":{");
            boolean f2 = true;
            for (Map.Entry<String, String> t : e.getValue().txt.entrySet()) {
                if (t.getKey() == null || t.getValue() == null) continue;
                if (!f2) b.append(',');
                f2 = false;
                b.append(quote(t.getKey())).append(':').append(quote(t.getValue()));
            }
            b.append("}}");
        }
        return b.append("]}").toString();
    }
}
