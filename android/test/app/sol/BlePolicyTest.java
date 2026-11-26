package app.sol;

import java.io.ByteArrayOutputStream;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** BlePolicy on a plain JDK (android/test/run.sh). */
public final class BlePolicyTest {
    private static int failed;

    private static void check(boolean ok, String what) {
        if (!ok) { failed++; System.err.println("FAIL: " + what); }
    }

    private static byte[] entry(int fill) {
        byte[] e = new byte[BlePolicy.ENTRY_SIZE];
        Arrays.fill(e, (byte) fill);
        e[0] = 1;
        return e;
    }

    private static void sd(ByteArrayOutputStream b, byte[] uuid, byte[] data) {
        b.write(1 + uuid.length + data.length);
        b.write(0x21);
        b.write(uuid, 0, uuid.length);
        b.write(data, 0, data.length);
    }

    public static void main(String[] args) {
        // The UUID goes least significant byte first, as Android writes it into an advert.
        byte[] u = BlePolicy.uuidLE(BlePolicy.SERVICE_UUID);
        check(u.length == 16 && (u[0] & 0xff) == 0x1b && (u[15] & 0xff) == 0x36, "uuid byte order");

        // Advertisement (flags + entry) and scan response (entry), another service, padding and
        // a truncated structure at the end: both Sol entries come out, nothing else.
        ByteArrayOutputStream b = new ByteArrayOutputStream();
        b.write(2); b.write(1); b.write(6);
        sd(b, u, entry(0x11));
        b.write(0); b.write(0); // padding up to the scan response
        sd(b, BlePolicy.uuidLE("00000000-0000-0000-0000-000000000001"), entry(0x22));
        sd(b, u, entry(0x33));
        sd(b, u, new byte[4]); // the right UUID, not an entry
        b.write(30); b.write(0x21); // truncated
        List<byte[]> es = BlePolicy.entries(b.toByteArray());
        check(es.size() == 2 && es.get(0)[1] == 0x11 && es.get(1)[1] == 0x33, "entries: " + es.size());
        check(BlePolicy.entries(null).isEmpty() && BlePolicy.entries(new byte[]{5, 1}).isEmpty(), "no entries");
        check(b.toByteArray().length > 31, "two structures need advert and scan response");
        // One structure fits a legacy advert next to the flags.
        check(3 + 2 + 16 + BlePolicy.ENTRY_SIZE == 31, "legacy advert size");

        // Records: new advertisers and entries are news, repeats are not; old entries expire.
        Map<String, BlePolicy.Record> seen = new LinkedHashMap<>();
        long now = 1_000_000;
        check(BlePolicy.put(seen, "dev1", es, now), "new advertiser");
        check(!BlePolicy.put(seen, "dev1", es, now + 1000), "repeat");
        check(BlePolicy.put(seen, "dev1", Collections.singletonList(entry(0x44)), now + 2000), "rotated entry");
        check(!BlePolicy.put(seen, "", es, now) && !BlePolicy.put(seen, "dev2", Collections.emptyList(), now), "nothing to record");
        check(BlePolicy.prune(seen, now + BlePolicy.RECORD_TTL_MS + 1000) == 1 && seen.get("dev1").entries.size() == 1, "prune entries");
        check(BlePolicy.prune(seen, now + 2 * BlePolicy.RECORD_TTL_MS + 2000) == 0, "prune records");
        for (int i = 0; i < BlePolicy.MAX_ENTRIES + 4; i++) BlePolicy.put(seen, "dev3", Collections.singletonList(entry(i)), now + i);
        check(seen.get("dev3").entries.size() == BlePolicy.MAX_ENTRIES, "entries cap");
        for (int i = 0; i < BlePolicy.MAX_RECORDS + 3; i++) BlePolicy.put(seen, "id" + i, es, now + 100 + i);
        check(BlePolicy.prune(seen, now + 200) == BlePolicy.MAX_RECORDS, "records cap");

        // The request the node reads.
        Map<String, BlePolicy.Record> one = new LinkedHashMap<>();
        BlePolicy.put(one, "a\"b", Collections.singletonList(entry(0x11)), now);
        check(BlePolicy.matchRequest(one).equals("{\"seen\":[{\"id\":\"a\\\"b\",\"data\":[\"AREREREREREREQ==\"]}]}"), "request " + BlePolicy.matchRequest(one));
        check(BlePolicy.adRequest(0x85).equals("{\"psm\":133}") && BlePolicy.adRequest(5).equals("{\"psm\":0}"), "ad request");

        // Rotation: two entries a slot, wrapping.
        check(Arrays.equals(BlePolicy.pair(4, 0), new int[]{0, 1}) && Arrays.equals(BlePolicy.pair(4, 1), new int[]{2, 3})
                && Arrays.equals(BlePolicy.pair(4, 2), new int[]{0, 1}) && BlePolicy.pair(0, 3) == null, "pair");

        // The bridge's first line.
        check(BlePolicy.preface(null).equals("SOLBLE1 in\n") && BlePolicy.preface("0a1b").equals("SOLBLE1 out 0a1b\n"), "preface");
        boolean threw = false;
        try { BlePolicy.preface("0a b\n"); } catch (IllegalArgumentException e) { threw = true; }
        check(threw, "a handle that could break the line");

        if (failed > 0) { System.err.println(failed + " BlePolicy checks failed"); System.exit(1); }
        System.out.println("ok BlePolicyTest");
    }
}
