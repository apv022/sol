package app.sol;

import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.UUID;

/**
 * The parsing, timing and bookkeeping of nearby over Bluetooth LE (Ble), with no Android types
 * so it is tested on a plain JDK (android/test). Every decision about keys, tags and links is the
 * node's (`libsol.so nearby ble-match`); this class only reads adverts, says when to ask the node
 * again and when to let go (design/PROTOCOL.md §Nearby "Bluetooth LE").
 */
final class BlePolicy {
    /** Sol's 128-bit service UUID (the node's nearby.BLEServiceUUID). */
    static final String SERVICE_UUID = "36360d3f-85e7-4adb-8ab0-1a8849b5fa1b";
    /** One entry: version byte, 6-byte tag, 3 masked bytes. */
    static final int ENTRY_SIZE = 10;
    /** The advert and scan response show the next pair of entries this often. */
    static final long ROTATE_MS = 3_000;
    /** Scanning runs this long, then rests SCAN_OFF_MS (low-power duty cycle). */
    static final long SCAN_ON_MS = 30_000;
    static final long SCAN_OFF_MS = 30_000;
    /** When the node says every kept device has an IP path, scanning rests this long. */
    static final long PAUSE_MS = 5 * 60_000;
    /** An entry older than this no longer counts (the phone left, or its entries rotated). */
    static final long RECORD_TTL_MS = 5 * 60_000;
    /** At most this many advertisers, and entries per advertiser, are handed to the node. */
    static final int MAX_RECORDS = 64;
    static final int MAX_ENTRIES = 16;
    /** An L2CAP channel that has not connected by then is given up. */
    static final long CONNECT_TIMEOUT_MS = 20_000;

    private BlePolicy() { }

    /** The UUID as it appears in an advert: 16 bytes, least significant first. */
    static byte[] uuidLE(String uuid) {
        UUID u = UUID.fromString(uuid);
        byte[] b = new byte[16];
        long lo = u.getLeastSignificantBits(), hi = u.getMostSignificantBits();
        for (int i = 0; i < 8; i++) { b[i] = (byte) (lo >>> (8 * i)); b[8 + i] = (byte) (hi >>> (8 * i)); }
        return b;
    }

    /**
     * The Sol entries in a raw advert (advertisement and scan response, as ScanRecord.getBytes
     * gives them): every 128-bit service data structure (AD type 0x21) under Sol's UUID whose data
     * is one entry. Android's ScanRecord keeps only one service data value per UUID, so the raw
     * bytes are read. Malformed structures end the parse; what came before them is kept.
     */
    static List<byte[]> entries(byte[] raw) {
        List<byte[]> out = new ArrayList<>();
        if (raw == null) return out;
        byte[] uuid = uuidLE(SERVICE_UUID);
        int i = 0;
        while (i < raw.length) {
            int len = raw[i] & 0xff;
            if (len == 0) { i++; continue; } // padding between advert and scan response
            if (i + 1 + len > raw.length) break;
            int type = raw[i + 1] & 0xff;
            if (type == 0x21 && len == 1 + 16 + ENTRY_SIZE) {
                boolean ours = true;
                for (int k = 0; k < 16 && ours; k++) ours = raw[i + 2 + k] == uuid[k];
                if (ours) {
                    byte[] e = new byte[ENTRY_SIZE];
                    System.arraycopy(raw, i + 2 + 16, e, 0, ENTRY_SIZE);
                    out.add(e);
                }
            }
            i += 1 + len;
        }
        return out;
    }

    /** The entries one advertiser sent lately (base64, when each was last seen). */
    static final class Record {
        final Map<String, Long> entries = new HashMap<>();
    }

    /**
     * Records the entries an advertiser sent now; returns whether that is news (a new advertiser
     * or entry), i.e. whether the node should be asked again.
     */
    static boolean put(Map<String, Record> seen, String id, List<byte[]> entries, long now) {
        if (id == null || id.isEmpty() || entries == null || entries.isEmpty()) return false;
        Record r = seen.get(id);
        boolean news = r == null;
        if (r == null) { r = new Record(); seen.put(id, r); }
        for (byte[] e : entries) {
            String k = Base64.getEncoder().encodeToString(e);
            Long old = r.entries.put(k, now);
            news |= old == null;
        }
        while (r.entries.size() > MAX_ENTRIES) r.entries.remove(oldest(r.entries));
        return news;
    }

    private static String oldest(Map<String, Long> m) {
        String k = null;
        long at = Long.MAX_VALUE;
        for (Map.Entry<String, Long> e : m.entrySet()) if (e.getValue() < at) { at = e.getValue(); k = e.getKey(); }
        return k;
    }

    /** Drops entries older than RECORD_TTL_MS, empty records, and caps the rest; returns how many remain. */
    static int prune(Map<String, Record> seen, long now) {
        for (Iterator<Map.Entry<String, Record>> it = seen.entrySet().iterator(); it.hasNext(); ) {
            Record r = it.next().getValue();
            r.entries.values().removeIf(at -> now - at >= RECORD_TTL_MS);
            if (r.entries.isEmpty()) it.remove();
        }
        while (seen.size() > MAX_RECORDS) {
            String drop = null;
            long at = Long.MAX_VALUE;
            for (Map.Entry<String, Record> e : seen.entrySet()) {
                long newest = 0;
                for (long t : e.getValue().entries.values()) newest = Math.max(newest, t);
                if (newest < at) { at = newest; drop = e.getKey(); }
            }
            seen.remove(drop);
        }
        return seen.size();
    }

    /** The `nearby ble-match` request: {"seen":[{"id":..., "data":[base64...]}]}. */
    static String matchRequest(Map<String, Record> seen) {
        StringBuilder b = new StringBuilder("{\"seen\":[");
        boolean first = true;
        for (Map.Entry<String, Record> e : seen.entrySet()) {
            if (!first) b.append(',');
            first = false;
            b.append("{\"id\":").append(WifiDirectPolicy.quote(e.getKey())).append(",\"data\":[");
            boolean f2 = true;
            for (String d : e.getValue().entries.keySet()) {
                if (!f2) b.append(',');
                f2 = false;
                b.append(WifiDirectPolicy.quote(d));
            }
            b.append("]}");
        }
        return b.append("]}").toString();
    }

    /**
     * The two entries (advertisement, scan response) to show in rotation slot {@code slot}, from
     * the node's list: slot 0 shows entries 0 and 1, slot 1 entries 2 and 3, and so on, wrapping.
     * Null for an empty list.
     */
    static int[] pair(int n, int slot) {
        if (n <= 0) return null;
        int a = (2 * slot) % n;
        return new int[]{a, (a + 1) % n};
    }

    /** The first line of a bridged link: "SOLBLE1 in\n", or "SOLBLE1 out <link>\n" (link: hex). */
    static String preface(String link) {
        if (link == null) return "SOLBLE1 in\n";
        for (char c : link.toCharArray()) {
            if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) throw new IllegalArgumentException("bad link handle");
        }
        return "SOLBLE1 out " + link + "\n";
    }

    /** The `nearby ble-ad` request. */
    static String adRequest(int psm) { return "{\"psm\":" + (psm >= 0x80 && psm <= 0xff ? psm : 0) + "}"; }
}
