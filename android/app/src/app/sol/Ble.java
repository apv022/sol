package app.sol;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.bluetooth.BluetoothAdapter;
import android.bluetooth.BluetoothDevice;
import android.bluetooth.BluetoothManager;
import android.bluetooth.BluetoothServerSocket;
import android.bluetooth.BluetoothSocket;
import android.bluetooth.le.AdvertiseData;
import android.bluetooth.le.AdvertisingSet;
import android.bluetooth.le.AdvertisingSetCallback;
import android.bluetooth.le.AdvertisingSetParameters;
import android.bluetooth.le.BluetoothLeAdvertiser;
import android.bluetooth.le.BluetoothLeScanner;
import android.bluetooth.le.ScanCallback;
import android.bluetooth.le.ScanFilter;
import android.bluetooth.le.ScanRecord;
import android.bluetooth.le.ScanResult;
import android.bluetooth.le.ScanSettings;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.os.ParcelUuid;
import android.util.Base64;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.Closeable;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

/**
 * Nearby over Bluetooth LE (design/PROTOCOL.md §Nearby "Bluetooth LE (Android, nearby step 3)"):
 * finds kept devices with Wi-Fi off or Wi-Fi Direct slow, and carries a session over an L2CAP
 * channel when no IP link can be made at all.
 *
 * The shell only drives the radio. It advertises the 10-byte entries the node makes (`libsol.so
 * nearby ble-ad`: rotating tags only kept devices recognise, the L2CAP server's PSM masked for
 * them), two at a time, rotating; scans for Sol's service UUID with low-power settings, duty
 * cycled; and hands the entries it saw to `libsol.so nearby ble-match`, which says, per advertiser,
 * whether to kick Wi-Fi Direct or open a channel (with a link handle). Every channel, opened or
 * accepted, is bridged to the node as one loopback TCP connection whose first line says which
 * ("SOLBLE1 out <link>" or "SOLBLE1 in"); the session handshake on it authenticates both ends. No
 * key, tag or device identity is handled here, and link handles are never logged.
 *
 * Runs only while the node serves and both nearby and its Bluetooth LE switch are on (config.json
 * "nearby", "ble"; the node's "nearby" event says when they change). Android 10+ only (L2CAP
 * channels need API 29). Radio callbacks run on the main looper; node commands, accepts, connects
 * and the bridges' copying run on their own threads.
 */
final class Ble {
    private static final String TAG = "Sol";
    private static final String CHANNEL = "nearby";
    private static final int NOTE_PERMISSION = 4;
    /** The runtime permissions Bluetooth LE needs on this phone. */
    static final String[] PERMISSIONS = Build.VERSION.SDK_INT >= 31
            ? new String[]{Manifest.permission.BLUETOOTH_SCAN, Manifest.permission.BLUETOOTH_ADVERTISE, Manifest.permission.BLUETOOTH_CONNECT}
            : new String[]{Manifest.permission.ACCESS_FINE_LOCATION};
    private static final ParcelUuid UUID = ParcelUuid.fromString(BlePolicy.SERVICE_UUID);

    private final NodeService svc;
    private final Handler main = new Handler(Looper.getMainLooper());
    private BluetoothAdapter adapter;
    private BroadcastReceiver receiver;
    private boolean running, radio;
    private volatile BluetoothServerSocket server;
    private int psm;
    private volatile int bridgePort;
    private AdvertisingSet advSet;
    private AdvertisingSetCallback advCb;
    private List<byte[]> adverts = new ArrayList<>();
    private int slot;
    private ScanCallback scanCb;
    private boolean scanning;
    private long pausedUntil;
    private String reported = "";
    private final Map<String, BlePolicy.Record> seen = new LinkedHashMap<>();
    private final Map<String, BluetoothDevice> devices = new HashMap<>();
    private final Set<Closeable> links = ConcurrentHashMap.newKeySet();
    private final Runnable rotate = this::rotate;
    private final Runnable refreshAd = this::advertise;
    private final Runnable scanToggle = this::scanToggle;
    private final Runnable decideSoon = this::decide;

    Ble(NodeService svc) { this.svc = svc; }

    /** Whether the runtime permissions Bluetooth LE needs have been granted. */
    static boolean permitted(Context c) {
        for (String p : PERMISSIONS) if (c.checkSelfPermission(p) != PackageManager.PERMISSION_GRANTED) return false;
        return true;
    }

    /** Whether config.json has nearby and Bluetooth LE on. */
    static boolean wanted(Context c) {
        try {
            File cfg = new File(NodeService.dataDir(c), "config.json");
            if (!cfg.exists()) return false;
            JSONObject j = new JSONObject(new String(Files.readAllBytes(cfg.toPath()), StandardCharsets.UTF_8));
            return j.optBoolean("nearby", true) && j.optBoolean("ble", false);
        } catch (IOException | JSONException e) {
            return false;
        }
    }

    /** The node serves: start if switched on. */
    void nodeReady() { main.post(() -> apply(wanted(svc))); }

    /** The node's "nearby" event: follow the switches. */
    void onNearby(JSONObject st) {
        JSONObject b = st.optJSONObject("ble");
        boolean on = st.optBoolean("enabled", false) && b != null && b.optBoolean("enabled", false);
        main.post(() -> apply(on));
    }

    /** The node stops: stop advertising and scanning, cut every link. */
    void stop() { main.post(() -> apply(false)); }

    private void apply(boolean on) {
        if (on && !running) start();
        else if (!on && running) shutdown(true);
        else if (!on) report("off", "");
    }

    private void start() {
        if (Build.VERSION.SDK_INT < 29) { report("unsupported", "needs Android 10 or later"); return; }
        if (!svc.getPackageManager().hasSystemFeature(PackageManager.FEATURE_BLUETOOTH_LE)) {
            report("unsupported", "this phone has no Bluetooth LE");
            return;
        }
        if (!permitted(svc)) {
            report("needs_permission", "open Sol to allow Bluetooth access");
            askPermission();
            return;
        }
        BluetoothManager bm = svc.getSystemService(BluetoothManager.class);
        adapter = bm == null ? null : bm.getAdapter();
        if (adapter == null) { report("unsupported", "this phone has no Bluetooth"); return; }
        running = true;
        receiver = new BroadcastReceiver() {
            @Override public void onReceive(Context c, Intent in) {
                int s = in.getIntExtra(BluetoothAdapter.EXTRA_STATE, BluetoothAdapter.STATE_OFF);
                if (s == BluetoothAdapter.STATE_ON) radioOn();
                else if (s == BluetoothAdapter.STATE_TURNING_OFF || s == BluetoothAdapter.STATE_OFF) radioOff();
            }
        };
        svc.registerReceiver(receiver, new IntentFilter(BluetoothAdapter.ACTION_STATE_CHANGED), Context.RECEIVER_NOT_EXPORTED);
        if (adapter.isEnabled()) radioOn();
        else report("bluetooth_off", "turn Bluetooth on");
    }

    /** Bluetooth is on: listen for channels, advertise and scan. */
    private void radioOn() {
        if (!running || radio) return;
        radio = true;
        try {
            server = adapter.listenUsingInsecureL2capChannel();
            psm = server.getPsm();
        } catch (IOException | SecurityException e) {
            Log.w(TAG, "ble: no L2CAP server: " + e.getClass().getSimpleName());
            server = null;
            psm = 0; // others can still connect to nobody; discovery still works
        }
        BluetoothServerSocket srv = server;
        if (srv != null) new Thread(() -> acceptLoop(srv), "sol-ble-accept").start();
        report("searching", "");
        pausedUntil = 0;
        advertise();
        scanToggle();
    }

    /** Bluetooth went off (or the feature stops): everything radio-bound ends. */
    private void radioOff() {
        if (!radio) return;
        radio = false;
        main.removeCallbacks(rotate);
        main.removeCallbacks(refreshAd);
        main.removeCallbacks(scanToggle);
        main.removeCallbacks(decideSoon);
        stopAdvertising();
        stopScan();
        BluetoothServerSocket srv = server;
        server = null;
        if (srv != null) try { srv.close(); } catch (IOException ignored) { }
        for (Closeable c : links) try { c.close(); } catch (IOException ignored) { }
        links.clear();
        seen.clear();
        devices.clear();
        if (running) report("bluetooth_off", "turn Bluetooth on");
    }

    private void shutdown(boolean off) {
        radioOff();
        running = false;
        if (receiver != null) { try { svc.unregisterReceiver(receiver); } catch (RuntimeException ignored) { } receiver = null; }
        if (off) report("off", "");
    }

    /** Asks the node for the entries to advertise (again when their window ends) and shows them. */
    private void advertise() {
        if (!radio) return;
        main.removeCallbacks(refreshAd);
        node(BlePolicy.adRequest(psm), out -> {
            if (!radio) return;
            long wait = 60_000;
            if (out != null) {
                try {
                    JSONObject j = new JSONObject(out);
                    bridgePort = j.optInt("bridge", 0);
                    JSONArray a = j.optJSONArray("adverts");
                    List<byte[]> es = new ArrayList<>();
                    for (int i = 0; a != null && i < a.length(); i++) {
                        byte[] e = Base64.decode(a.getString(i), Base64.DEFAULT);
                        if (e.length == BlePolicy.ENTRY_SIZE) es.add(e);
                    }
                    adverts = es;
                    wait = Math.max(5_000, j.optLong("expires") * 1000 - System.currentTimeMillis() + 1000);
                } catch (JSONException | IllegalArgumentException e) {
                    Log.w(TAG, "ble: bad ad reply");
                }
            }
            main.postDelayed(refreshAd, wait);
            if (adverts.isEmpty()) stopAdvertising();
            else startAdvertising();
        }, "nearby", "ble-ad", "-");
    }

    private AdvertiseData data(byte[] entry) {
        return new AdvertiseData.Builder().addServiceData(UUID, entry)
                .setIncludeDeviceName(false).setIncludeTxPowerLevel(false).build();
    }

    private void startAdvertising() {
        slot = 0;
        int[] p = BlePolicy.pair(adverts.size(), slot);
        if (p == null) return;
        if (advCb != null) { if (advSet != null) rotate(); return; } // running, or starting
        BluetoothLeAdvertiser adv = adapter.getBluetoothLeAdvertiser();
        if (adv == null) { Log.i(TAG, "ble: this phone cannot advertise; scanning only"); return; }
        AdvertisingSetParameters params = new AdvertisingSetParameters.Builder().setLegacyMode(true)
                .setConnectable(true).setScannable(true).setInterval(AdvertisingSetParameters.INTERVAL_MEDIUM)
                .setTxPowerLevel(AdvertisingSetParameters.TX_POWER_MEDIUM).build();
        advCb = new AdvertisingSetCallback() {
            @Override public void onAdvertisingSetStarted(AdvertisingSet set, int txPower, int status) {
                if (status != AdvertisingSetCallback.ADVERTISE_SUCCESS || set == null) {
                    Log.i(TAG, "ble: advertising did not start (" + status + ")");
                    advCb = null;
                    return;
                }
                advSet = set;
                if (!radio) { stopAdvertising(); return; }
                main.removeCallbacks(rotate);
                main.postDelayed(rotate, BlePolicy.ROTATE_MS);
            }
            @Override public void onAdvertisingSetStopped(AdvertisingSet set) { if (advSet == set) advSet = null; }
        };
        try {
            adv.startAdvertisingSet(params, data(adverts.get(p[0])), data(adverts.get(p[1])), null, null, advCb, main);
        } catch (SecurityException | IllegalArgumentException e) {
            Log.w(TAG, "ble: cannot advertise: " + e.getClass().getSimpleName());
            advCb = null;
        }
    }

    /** Shows the next pair of entries. */
    private void rotate() {
        main.removeCallbacks(rotate);
        if (!radio || advSet == null || adverts.isEmpty()) return;
        int[] p = BlePolicy.pair(adverts.size(), ++slot);
        try {
            advSet.setAdvertisingData(data(adverts.get(p[0])));
            advSet.setScanResponseData(data(adverts.get(p[1])));
        } catch (SecurityException | IllegalArgumentException e) {
            Log.w(TAG, "ble: cannot rotate the advert: " + e.getClass().getSimpleName());
        }
        if (adverts.size() > 2) main.postDelayed(rotate, BlePolicy.ROTATE_MS);
    }

    private void stopAdvertising() {
        main.removeCallbacks(rotate);
        BluetoothLeAdvertiser adv = adapter == null ? null : adapter.getBluetoothLeAdvertiser();
        if (adv != null && advCb != null) try { adv.stopAdvertisingSet(advCb); } catch (SecurityException | IllegalStateException ignored) { }
        advCb = null;
        advSet = null;
    }

    /** The scan's duty cycle: SCAN_ON_MS on, SCAN_OFF_MS off, longer while the node says pause. */
    private void scanToggle() {
        main.removeCallbacks(scanToggle);
        if (!radio) return;
        long now = System.currentTimeMillis();
        if (scanning) {
            stopScan();
            main.postDelayed(scanToggle, BlePolicy.SCAN_OFF_MS);
        } else if (now < pausedUntil) {
            main.postDelayed(scanToggle, pausedUntil - now);
        } else {
            startScan();
            main.postDelayed(scanToggle, BlePolicy.SCAN_ON_MS);
        }
    }

    private void startScan() {
        BluetoothLeScanner sc = adapter.getBluetoothLeScanner();
        if (sc == null) return;
        scanCb = new ScanCallback() {
            @Override public void onScanResult(int type, ScanResult r) { seenResult(r); }
            @Override public void onBatchScanResults(List<ScanResult> rs) { for (ScanResult r : rs) seenResult(r); }
            @Override public void onScanFailed(int code) { Log.i(TAG, "ble: scan failed (" + code + ")"); scanning = false; }
        };
        ScanFilter f = new ScanFilter.Builder().setServiceData(UUID, new byte[]{1}, new byte[]{(byte) 0xff}).build();
        ScanSettings s = new ScanSettings.Builder().setScanMode(ScanSettings.SCAN_MODE_LOW_POWER).build();
        try {
            sc.startScan(Collections.singletonList(f), s, scanCb);
            scanning = true;
        } catch (SecurityException | IllegalStateException e) {
            Log.w(TAG, "ble: cannot scan: " + e.getClass().getSimpleName());
            if (e instanceof SecurityException) report("needs_permission", "open Sol to allow Bluetooth access");
        }
    }

    private void stopScan() {
        BluetoothLeScanner sc = adapter == null ? null : adapter.getBluetoothLeScanner();
        if (sc != null && scanCb != null) try { sc.stopScan(scanCb); } catch (SecurityException | IllegalStateException ignored) { }
        scanCb = null;
        scanning = false;
    }

    private void seenResult(ScanResult r) {
        if (!radio || r == null || r.getDevice() == null) return;
        ScanRecord rec = r.getScanRecord();
        List<byte[]> es = BlePolicy.entries(rec == null ? null : rec.getBytes());
        String id = r.getDevice().getAddress();
        devices.put(id, r.getDevice());
        if (BlePolicy.put(seen, id, es, System.currentTimeMillis())) {
            main.removeCallbacks(decideSoon);
            main.postDelayed(decideSoon, 1000);
        }
    }

    /** Hands the entries seen to the node and does what it says. */
    private void decide() {
        if (!radio) return;
        BlePolicy.prune(seen, System.currentTimeMillis());
        devices.keySet().retainAll(seen.keySet());
        if (seen.isEmpty()) return;
        node(BlePolicy.matchRequest(seen), out -> {
            if (!radio || out == null) return;
            try {
                JSONObject p = new JSONObject(out);
                if (!p.optBoolean("scan", true)) {
                    pausedUntil = System.currentTimeMillis() + BlePolicy.PAUSE_MS;
                    if (scanning) { stopScan(); main.removeCallbacks(scanToggle); main.postDelayed(scanToggle, BlePolicy.PAUSE_MS); }
                }
                JSONArray acts = p.optJSONArray("actions");
                for (int i = 0; acts != null && i < acts.length(); i++) {
                    JSONObject a = acts.getJSONObject(i);
                    String what = a.optString("do");
                    if ("wifi-direct".equals(what)) {
                        svc.wifiDirect.kick();
                    } else if ("connect".equals(what)) {
                        BluetoothDevice d = devices.get(a.optString("id"));
                        String link = a.optString("link");
                        int to = a.optInt("psm");
                        if (d != null && !link.isEmpty() && to > 0) new Thread(() -> connect(d, to, link), "sol-ble-connect").start();
                    }
                }
            } catch (JSONException e) {
                Log.w(TAG, "ble: bad match reply");
            }
        }, "nearby", "ble-match", "-");
    }

    /** Opens an L2CAP channel to d and bridges it (off the main thread). */
    private void connect(BluetoothDevice d, int to, String link) {
        BluetoothSocket s = null;
        try {
            s = d.createInsecureL2capChannel(to);
            BluetoothSocket fs = s;
            Runnable cut = () -> { try { fs.close(); } catch (IOException ignored) { } };
            main.postDelayed(cut, BlePolicy.CONNECT_TIMEOUT_MS);
            s.connect();
            main.removeCallbacks(cut);
            bridge(s, link);
        } catch (IOException | SecurityException | IllegalArgumentException e) {
            Log.i(TAG, "ble: channel did not open: " + e.getClass().getSimpleName());
            if (s != null) try { s.close(); } catch (IOException ignored) { }
        }
    }

    private void acceptLoop(BluetoothServerSocket srv) {
        while (server == srv) {
            try {
                BluetoothSocket s = srv.accept();
                new Thread(() -> bridge(s, null), "sol-ble-in").start();
            } catch (IOException e) {
                return; // closed
            }
        }
    }

    /**
     * Joins a channel to the node: one loopback connection to the bridge port, its first line
     * saying which kind (link null: accepted), then the bytes both ways until either end closes.
     */
    private void bridge(BluetoothSocket bs, String link) {
        int port = bridgePort;
        Socket tcp = null;
        try {
            if (port <= 0 || server == null && link == null) throw new IOException("not bridging");
            tcp = new Socket(InetAddress.getLoopbackAddress(), port);
            tcp.setTcpNoDelay(true);
            tcp.getOutputStream().write(BlePolicy.preface(link).getBytes(StandardCharsets.US_ASCII));
        } catch (IOException | IllegalArgumentException e) {
            try { bs.close(); } catch (IOException ignored) { }
            if (tcp != null) try { tcp.close(); } catch (IOException ignored) { }
            return;
        }
        Socket t = tcp;
        links.add(bs);
        links.add(t);
        main.post(this::reportLinks);
        Thread up = new Thread(() -> copy(bs, t, bs, t), "sol-ble-up");
        up.start();
        copy(t, bs, bs, t);
        try { up.join(); } catch (InterruptedException ignored) { }
        links.remove(bs);
        links.remove(t);
        main.post(this::reportLinks);
    }

    private static void copy(Object from, Object to, BluetoothSocket bs, Socket t) {
        byte[] buf = new byte[4096];
        try {
            InputStream in = from instanceof Socket ? ((Socket) from).getInputStream() : ((BluetoothSocket) from).getInputStream();
            OutputStream out = to instanceof Socket ? ((Socket) to).getOutputStream() : ((BluetoothSocket) to).getOutputStream();
            int n;
            while ((n = in.read(buf)) >= 0) { out.write(buf, 0, n); out.flush(); }
        } catch (IOException ignored) {
        } finally {
            try { bs.close(); } catch (IOException ignored) { }
            try { t.close(); } catch (IOException ignored) { }
        }
    }

    private void reportLinks() {
        if (!radio) return;
        int n = links.size() / 2;
        if (n > 0) report("connected", n + (n == 1 ? " Bluetooth link" : " Bluetooth links"));
        else report("searching", "");
    }

    /** Tells the node (and so `sol status` and the Sol app) how Bluetooth LE is doing. */
    private void report(String st, String detail) {
        String k = st + "\n" + detail;
        if (k.equals(reported)) return;
        reported = k;
        node(null, null, "nearby", "ble-report", st, detail);
    }

    /** Turned on by an agent or another device without the permissions: ask the person. */
    private void askPermission() {
        NotificationManager nm = svc.getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, "Nearby", NotificationManager.IMPORTANCE_DEFAULT));
        Intent open = new Intent(svc, MainActivity.class).putExtra(MainActivity.EXTRA_BLE_ASK, true)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        PendingIntent pi = PendingIntent.getActivity(svc, 4, open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        nm.notify(NOTE_PERMISSION, new Notification.Builder(svc, CHANNEL)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle("Allow Nearby devices for Bluetooth")
                .setContentText("Lets Sol find your devices and sync small things over Bluetooth. Tap to allow.")
                .setContentIntent(pi).setAutoCancel(true).build());
    }

    /** Clears the permission notification (the person allowed it). */
    static void permissionGiven(Context c) {
        c.getSystemService(NotificationManager.class).cancel(NOTE_PERMISSION);
    }

    private interface Out { void got(String out); }

    /** Runs `nearby <op>` off the main thread (stdin: the request, if any). */
    private void node(String stdin, Out then, String... args) {
        new Thread(() -> {
            String[] r = NodeService.cli2In(svc, stdin == null ? "" : stdin, args);
            String out = r[0].equals("0") ? r[1] : null;
            if (out == null) Log.w(TAG, "ble: " + args[1] + " failed (" + r[0] + ")");
            if (then != null) {
                int i = out == null ? -1 : out.indexOf('{');
                String json = i < 0 ? null : out.substring(i);
                main.post(() -> then.got(json));
            }
        }, "sol-ble").start();
    }
}
