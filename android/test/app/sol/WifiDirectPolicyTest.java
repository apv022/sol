package app.sol;

import java.util.Arrays;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;

/** WifiDirectPolicy on a plain JDK (android/test/run.sh). */
public final class WifiDirectPolicyTest {
    private static int failed;

    private static void check(boolean ok, String what) {
        if (!ok) { failed++; System.err.println("FAIL: " + what); }
    }

    public static void main(String[] args) {
        Map<String, WifiDirectPolicy.Record> seen = new LinkedHashMap<>();
        Map<String, String> txt = new HashMap<>();
        txt.put("v", "1");
        txt.put("t0", "abc");
        long now = 1_000_000;
        // Only Sol's service counts; the first record and a changed one are news, a repeat is not.
        check(!WifiDirectPolicy.put(seen, "aa:bb", "printer._ipp._tcp.local.", txt, now), "other service");
        check(WifiDirectPolicy.put(seen, "aa:bb", "sol._sol._udp.local.", txt, now), "new record");
        check(!WifiDirectPolicy.put(seen, "aa:bb", "sol._sol._udp.local.", txt, now + 1000), "repeat");
        Map<String, String> txt2 = new HashMap<>(txt);
        txt2.put("t0", "xyz");
        check(WifiDirectPolicy.put(seen, "aa:bb", "sol._sol._udp.local.", txt2, now + 2000), "changed record");
        check(!WifiDirectPolicy.put(seen, null, "sol._sol._udp.local.", txt, now), "no id");
        // Records expire.
        WifiDirectPolicy.put(seen, "cc:dd", "sol._sol._udp.local.", txt, now + WifiDirectPolicy.RECORD_TTL_MS);
        check(WifiDirectPolicy.prune(seen, now + WifiDirectPolicy.RECORD_TTL_MS + 2000) == 1 && seen.containsKey("cc:dd"), "prune");
        for (int i = 0; i < WifiDirectPolicy.MAX_RECORDS + 5; i++) WifiDirectPolicy.put(seen, "id" + i, "sol._sol._udp.local.", txt, now + 10_000 + i);
        check(WifiDirectPolicy.prune(seen, now + 20_000) == WifiDirectPolicy.MAX_RECORDS, "cap");

        // Idle teardown.
        check(!WifiDirectPolicy.ownerIdle(1, 0, WifiDirectPolicy.IDLE_MS * 10), "clients keep it");
        check(!WifiDirectPolicy.ownerIdle(0, now, now + WifiDirectPolicy.IDLE_MS - 1), "not yet");
        check(WifiDirectPolicy.ownerIdle(0, now, now + WifiDirectPolicy.IDLE_MS), "idle");

        // The group's address.
        check("192.168.49.1".equals(WifiDirectPolicy.pickIpv4(Arrays.asList("fe80::1", "/10.0.0.5", "/192.168.49.1"))), "p2p subnet first");
        check("10.0.0.5".equals(WifiDirectPolicy.pickIpv4(Arrays.asList("127.0.0.1", "169.254.1.1", "10.0.0.5"))), "any private");
        check(WifiDirectPolicy.pickIpv4(Arrays.asList("fe80::1", "999.1.1.1", "239.1.1.1", null)) == null, "none");

        // The match request is valid JSON with escaped values.
        Map<String, WifiDirectPolicy.Record> one = new LinkedHashMap<>();
        Map<String, String> t = new LinkedHashMap<>();
        t.put("v", "1");
        t.put("t0", "a\"b\\c\n");
        one.put("aa:bb", new WifiDirectPolicy.Record(t, now));
        String req = WifiDirectPolicy.matchRequest("none", one);
        check(req.startsWith("{\"state\":\"none\",\"seen\":[{\"id\":\"aa:bb\",\"txt\":{"), "request: " + req);
        check(req.contains("\"t0\":\"a\\\"b\\\\c\\u000a\""), "escaping: " + req);
        check(WifiDirectPolicy.matchRequest("owner", new HashMap<>()).equals("{\"state\":\"owner\",\"seen\":[]}"), "empty");

        if (failed > 0) { System.err.println(failed + " failed"); System.exit(1); }
        System.out.println("ok WifiDirectPolicyTest");
    }
}
