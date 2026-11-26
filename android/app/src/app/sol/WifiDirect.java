package app.sol;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.net.wifi.p2p.WifiP2pConfig;
import android.net.wifi.p2p.WifiP2pGroup;
import android.net.wifi.p2p.WifiP2pManager;
import android.net.wifi.p2p.nsd.WifiP2pDnsSdServiceInfo;
import android.net.wifi.p2p.nsd.WifiP2pDnsSdServiceRequest;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.net.DatagramSocket;
import java.net.Inet4Address;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.NetworkInterface;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Nearby over Wi-Fi Direct (design/PROTOCOL.md §Nearby "Wi-Fi Direct (Android)"): links two
 * phones that share no network, so the node's beacons and then its sessions run over the group.
 *
 * The shell only drives the radio. It advertises a DNS-SD service whose TXT record the node
 * makes (`libsol.so nearby ad`: rotating tags only kept devices recognise, plus the group's
 * credentials masked for them when this phone owns a group), discovers the same service on
 * other phones, and hands what it saw to `libsol.so nearby match`, which says whether to lead
 * (create a group with credentials it chose), join (a kept device's group) or wait. No key, tag
 * or device identity is handled here. Once the group's interface has an address it is handed to
 * the node (`nearby interfaces`, then `network-changed`), because Android 11+ does not let the
 * node list interfaces. The passphrase only ever travels on stdin and is never logged.
 *
 * Runs only while the node serves and both nearby and its Wi-Fi Direct switch are on (config.json
 * "nearby", "wifi_direct"; the node's "nearby" event says when they change). Android 10+ only:
 * joining with known credentials, without a dialog on either phone, needs WifiP2pConfig.Builder.
 * Everything runs on the main looper except the node commands, which run on their own thread.
 */
final class WifiDirect {
    private static final String TAG = "Sol";
    private static final String CHANNEL = "nearby";
    private static final int NOTE_PERMISSION = 3;
    /** The Android permission Wi-Fi Direct needs on this phone. */
    static final String PERMISSION = Build.VERSION.SDK_INT >= 33
            ? Manifest.permission.NEARBY_WIFI_DEVICES : Manifest.permission.ACCESS_FINE_LOCATION;

    private final NodeService svc;
    private final Handler main = new Handler(Looper.getMainLooper());
    private WifiP2pManager mgr;
    private WifiP2pManager.Channel ch;
    private BroadcastReceiver receiver;
    private boolean running;
    /** "none", "owner" or "client". */
    private String state = "none";
    /** The credentials of the group this phone owns (never logged). */
    private String ssid, pass;
    private long pendingUntil;
    private long lastMemberAt;
    private String reported = "";
    private final Map<String, WifiDirectPolicy.Record> seen = new LinkedHashMap<>();
    private final Runnable rediscover = this::discover;
    private final Runnable refreshAd = this::advertise;
    private final Runnable decideSoon = this::decide;
    private final Runnable idleCheck = this::checkIdle;

    WifiDirect(NodeService svc) { this.svc = svc; }

    /** Whether the permission Wi-Fi Direct needs has been granted. */
    static boolean permitted(Context c) {
        return c.checkSelfPermission(PERMISSION) == PackageManager.PERMISSION_GRANTED;
    }

    /** Whether config.json has nearby and Wi-Fi Direct on. */
    static boolean wanted(Context c) {
        try {
            File cfg = new File(NodeService.dataDir(c), "config.json");
            if (!cfg.exists()) return false;
            JSONObject j = new JSONObject(new String(Files.readAllBytes(cfg.toPath()), StandardCharsets.UTF_8));
            return j.optBoolean("nearby", true) && j.optBoolean("wifi_direct", false);
        } catch (java.io.IOException | JSONException e) {
            return false;
        }
    }

    /** The node serves: start if switched on. */
    void nodeReady() { main.post(() -> apply(wanted(svc))); }

    /** The node's "nearby" event: follow the switches. */
    void onNearby(JSONObject st) {
        JSONObject wd = st.optJSONObject("wifi_direct");
        boolean on = st.optBoolean("enabled", false) && wd != null && wd.optBoolean("enabled", false);
        main.post(() -> apply(on));
    }

    /** The node stops: leave the group and stop advertising. */
    void stop() { main.post(() -> apply(false)); }

    private void apply(boolean on) {
        if (on && !running) start();
        else if (!on && running) shutdown(true);
        else if (!on) report("off", "");
    }

    private void start() {
        if (Build.VERSION.SDK_INT < 29) { report("unsupported", "needs Android 10 or later"); return; }
        if (!svc.getPackageManager().hasSystemFeature(PackageManager.FEATURE_WIFI_DIRECT)) {
            report("unsupported", "this phone has no Wi-Fi Direct");
            return;
        }
        if (!permitted(svc)) {
            report("needs_permission", "open Sol to allow Nearby devices");
            askPermission();
            return;
        }
        mgr = svc.getSystemService(WifiP2pManager.class);
        ch = mgr == null ? null : mgr.initialize(svc, Looper.getMainLooper(), () -> main.post(this::channelLost));
        if (ch == null) { report("unsupported", "Wi-Fi Direct is not available"); return; }
        running = true;
        state = "none";
        receiver = new BroadcastReceiver() {
            @Override public void onReceive(Context c, Intent in) {
                if (WifiP2pManager.WIFI_P2P_STATE_CHANGED_ACTION.equals(in.getAction())) {
                    boolean en = in.getIntExtra(WifiP2pManager.EXTRA_WIFI_STATE, 0) == WifiP2pManager.WIFI_P2P_STATE_ENABLED;
                    if (!en) report("wifi_off", "turn Wi-Fi on (no network needed)");
                    else { report("searching", ""); advertise(); discover(); }
                } else if (WifiP2pManager.WIFI_P2P_CONNECTION_CHANGED_ACTION.equals(in.getAction())) {
                    groupChanged();
                }
            }
        };
        IntentFilter f = new IntentFilter();
        f.addAction(WifiP2pManager.WIFI_P2P_STATE_CHANGED_ACTION);
        f.addAction(WifiP2pManager.WIFI_P2P_CONNECTION_CHANGED_ACTION);
        svc.registerReceiver(receiver, f, Context.RECEIVER_NOT_EXPORTED);
        try {
            mgr.setDnsSdResponseListeners(ch, (name, type, dev) -> { },
                    (domain, txt, dev) -> {
                        if (dev != null && WifiDirectPolicy.put(seen, dev.deviceAddress, domain, txt, System.currentTimeMillis())) {
                            main.removeCallbacks(decideSoon);
                            main.postDelayed(decideSoon, 1000);
                        }
                    });
        } catch (SecurityException e) {
            shutdown(false);
            report("needs_permission", "open Sol to allow Nearby devices");
            return;
        }
        // The P2P state broadcast is sticky: registering delivers it at once, and "enabled"
        // starts advertising and discovering (doing it here too only makes the framework busy).
    }

    /** Leaves the group (if any), stops advertising and discovering, and tells the node. */
    private void shutdown(boolean off) {
        running = false;
        main.removeCallbacks(rediscover);
        main.removeCallbacks(refreshAd);
        main.removeCallbacks(decideSoon);
        main.removeCallbacks(idleCheck);
        if (receiver != null) { try { svc.unregisterReceiver(receiver); } catch (RuntimeException ignored) { } receiver = null; }
        if (mgr != null && ch != null) {
            try {
                mgr.removeGroup(ch, null);
                mgr.clearLocalServices(ch, null);
                mgr.clearServiceRequests(ch, null);
                mgr.stopPeerDiscovery(ch, null);
            } catch (SecurityException ignored) { }
            if (Build.VERSION.SDK_INT >= 27) ch.close();
        }
        ch = null;
        state = "none";
        ssid = pass = null;
        seen.clear();
        node(null, "nearby", "interfaces");
        if (off) report("off", "");
    }

    private void channelLost() {
        if (!running) return;
        shutdown(false);
        report("failed", "Android closed the Wi-Fi Direct channel");
    }

    /** Registers this phone's ad (re-registered when its tags' window ends). */
    private void advertise() {
        if (!running) return;
        main.removeCallbacks(refreshAd);
        String in = ssid == null ? "{}" : "{\"group\":{\"ssid\":" + WifiDirectPolicy.quote(ssid)
                + ",\"pass\":" + WifiDirectPolicy.quote(pass) + "}}";
        node(in, out -> {
            if (!running || out == null) return;
            try {
                JSONObject j = new JSONObject(out);
                JSONObject t = j.getJSONObject("txt");
                Map<String, String> txt = new HashMap<>();
                for (Iterator<String> it = t.keys(); it.hasNext(); ) { String k = it.next(); txt.put(k, t.getString(k)); }
                WifiP2pDnsSdServiceInfo info = WifiP2pDnsSdServiceInfo.newInstance("sol", "_sol._udp", txt);
                mgr.clearLocalServices(ch, new Listener("clear ad", () -> mgr.addLocalService(ch, info, new Listener("advertise", null))));
                long wait = Math.max(5_000, j.optLong("expires") * 1000 - System.currentTimeMillis() + 1000);
                main.postDelayed(refreshAd, wait);
            } catch (JSONException | SecurityException e) {
                Log.w(TAG, "wifi-direct: cannot advertise: " + e.getClass().getSimpleName());
            }
        }, "nearby", "ad", "-");
    }

    /** Asks Android for Sol's service on nearby phones, again every couple of minutes. */
    private void discover() {
        if (!running) return;
        main.removeCallbacks(rediscover);
        main.postDelayed(rediscover, WifiDirectPolicy.DISCOVER_EVERY_MS);
        try {
            mgr.clearServiceRequests(ch, new Listener("clear requests", () ->
                    mgr.addServiceRequest(ch, WifiP2pDnsSdServiceRequest.newInstance("_sol._udp"), new Listener("request", () ->
                            mgr.discoverServices(ch, new Listener("discover", null))))));
        } catch (SecurityException e) {
            report("needs_permission", "open Sol to allow Nearby devices");
        }
        main.removeCallbacks(decideSoon);
        main.postDelayed(decideSoon, 1000); // records that aged out may change the decision
    }

    /** Hands the records seen to the node and does what it says. */
    private void decide() {
        if (!running) return;
        long now = System.currentTimeMillis();
        WifiDirectPolicy.prune(seen, now);
        if (seen.isEmpty() || now < pendingUntil) return;
        String req = WifiDirectPolicy.matchRequest(state, seen);
        node(req, out -> {
            if (!running || out == null) return;
            try {
                JSONObject p = new JSONObject(out);
                String role = p.optString("role");
                JSONObject g = p.optJSONObject("group");
                if (!"none".equals(state) || System.currentTimeMillis() < pendingUntil || g == null) return;
                if (Build.VERSION.SDK_INT < 29) return;
                WifiP2pConfig cfg = new WifiP2pConfig.Builder().setNetworkName(g.getString("ssid"))
                        .setPassphrase(g.getString("pass")).enablePersistentMode(false).build();
                pendingUntil = System.currentTimeMillis() + WifiDirectPolicy.PENDING_MS;
                if ("lead".equals(role)) {
                    ssid = g.getString("ssid");
                    pass = g.getString("pass");
                    mgr.createGroup(ch, cfg, new Listener("create group", null));
                } else if ("join".equals(role)) {
                    mgr.connect(ch, cfg, new Listener("join group", null));
                } else {
                    pendingUntil = 0;
                }
            } catch (JSONException | IllegalArgumentException | SecurityException e) {
                pendingUntil = 0;
                Log.w(TAG, "wifi-direct: " + e.getClass().getSimpleName());
            }
        }, "nearby", "match", "-");
    }

    /** The group formed, changed or went away: tell the node where beacons can go now. */
    private void groupChanged() {
        if (!running) return;
        try {
            mgr.requestGroupInfo(ch, g -> main.post(() -> onGroup(g)));
        } catch (SecurityException e) {
            report("needs_permission", "open Sol to allow Nearby devices");
        }
    }

    private void onGroup(WifiP2pGroup g) {
        if (!running) return;
        String was = state;
        if (g == null) {
            state = "none";
            ssid = pass = null;
            pendingUntil = 0;
            node(null, "nearby", "interfaces");
            if (!"none".equals(was)) { report("searching", ""); advertise(); discover(); }
            return;
        }
        pendingUntil = 0;
        state = g.isGroupOwner() ? "owner" : "client";
        if (!g.isGroupOwner()) { ssid = pass = null; }
        int clients = g.getClientList() == null ? 0 : g.getClientList().size();
        long now = System.currentTimeMillis();
        if (clients > 0 || !state.equals(was)) lastMemberAt = now;
        String ip = groupAddress(g);
        if (ip != null) {
            node(null, "nearby", "interfaces", ip);
            node(null, "network-changed", "wifi-direct");
        }
        report("connected", g.isGroupOwner()
                ? "owner of a group with " + clients + (clients == 1 ? " device" : " devices")
                : "in a nearby phone's group");
        if (!state.equals(was)) advertise();
        if (g.isGroupOwner()) {
            main.removeCallbacks(idleCheck);
            main.postDelayed(idleCheck, WifiDirectPolicy.IDLE_MS + 1000);
        }
    }

    /** The owner removes a group nobody has been in for a while. */
    private void checkIdle() {
        if (!running || !"owner".equals(state)) return;
        try {
            mgr.requestGroupInfo(ch, g -> main.post(() -> {
                if (g == null || !g.isGroupOwner()) return;
                int clients = g.getClientList() == null ? 0 : g.getClientList().size();
                long now = System.currentTimeMillis();
                if (clients > 0) lastMemberAt = now;
                if (WifiDirectPolicy.ownerIdle(clients, lastMemberAt, now)) {
                    Log.i(TAG, "wifi-direct: nobody in the group; removing it");
                    mgr.removeGroup(ch, new Listener("remove group", null));
                } else {
                    main.postDelayed(idleCheck, WifiDirectPolicy.IDLE_MS);
                }
            }));
        } catch (SecurityException ignored) { }
    }

    /**
     * This phone's IPv4 address in the group. The interface's own addresses when Android lets
     * the app read them; otherwise the address a socket towards the group owner would use (no
     * interface listing needed), or the owner's fixed 192.168.49.1.
     */
    private static String groupAddress(WifiP2pGroup g) {
        List<String> addrs = new ArrayList<>();
        try {
            NetworkInterface ni = g.getInterface() == null ? null : NetworkInterface.getByName(g.getInterface());
            if (ni != null) for (InetAddress a : Collections.list(ni.getInetAddresses())) if (a instanceof Inet4Address) addrs.add(a.getHostAddress());
        } catch (java.net.SocketException | RuntimeException ignored) { }
        String ip = WifiDirectPolicy.pickIpv4(addrs);
        if (ip != null) return ip;
        if (g.isGroupOwner()) return "192.168.49.1";
        try (DatagramSocket s = new DatagramSocket()) {
            s.connect(new InetSocketAddress("192.168.49.1", 9));
            InetAddress a = s.getLocalAddress();
            if (a instanceof Inet4Address) return WifiDirectPolicy.pickIpv4(Collections.singletonList(a.getHostAddress()));
        } catch (java.io.IOException | RuntimeException ignored) { }
        return null;
    }

    /** Tells the node (and so `sol status` and the Sol app) how Wi-Fi Direct is doing. */
    private void report(String st, String detail) {
        String k = st + "\n" + detail;
        if (k.equals(reported)) return;
        reported = k;
        node(null, "nearby", "report", st, detail);
    }

    /** Turned on by an agent or another device without the permission: ask the person. */
    private void askPermission() {
        NotificationManager nm = svc.getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, "Nearby", NotificationManager.IMPORTANCE_DEFAULT));
        Intent open = new Intent(svc, MainActivity.class).putExtra(MainActivity.EXTRA_WIFI_DIRECT_ASK, true)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        PendingIntent pi = PendingIntent.getActivity(svc, 3, open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        nm.notify(NOTE_PERMISSION, new Notification.Builder(svc, CHANNEL)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle("Allow Nearby devices for Wi-Fi Direct")
                .setContentText("Lets Sol link phones with no network. Tap to allow.")
                .setContentIntent(pi).setAutoCancel(true).build());
    }

    /** Clears the permission notification (the person allowed it). */
    static void permissionGiven(Context c) {
        c.getSystemService(NotificationManager.class).cancel(NOTE_PERMISSION);
    }

    private interface Out { void got(String out); }

    /** Runs a node command off the main thread (stdin: the request, if any). */
    private void node(String stdin, String... args) { node(stdin, null, args); }

    private void node(String stdin, Out then, String... args) {
        new Thread(() -> {
            // `nearby <op>` is a two-word command (--dir after both); network-changed is one word.
            String[] r = args[0].equals("nearby") ? NodeService.cli2In(svc, stdin == null ? "" : stdin, args)
                    : NodeService.cli(svc, args);
            String out = r[0].equals("0") ? r[1] : null;
            if (out == null) Log.w(TAG, "wifi-direct: " + args[0] + (args.length > 1 ? " " + args[1] : "") + " failed (" + r[0] + ")");
            if (then != null) {
                int i = out == null ? -1 : out.indexOf('{');
                String json = i < 0 ? null : out.substring(i);
                main.post(() -> then.got(json));
            }
        }, "sol-wifi-direct").start();
    }

    /** Logs a WifiP2pManager failure by reason (never with data) and runs next on success. */
    private final class Listener implements WifiP2pManager.ActionListener {
        private final String what;
        private final Runnable next;
        Listener(String what, Runnable next) { this.what = what; this.next = next; }
        @Override public void onSuccess() { if (next != null && running) main.post(() -> { try { next.run(); } catch (SecurityException ignored) { } }); }
        @Override public void onFailure(int reason) {
            String why = reason == WifiP2pManager.P2P_UNSUPPORTED ? "unsupported"
                    : reason == WifiP2pManager.BUSY ? "busy" : reason == WifiP2pManager.NO_SERVICE_REQUESTS ? "no service requests" : "error";
            Log.i(TAG, "wifi-direct: " + what + " failed: " + why);
            if (reason == WifiP2pManager.P2P_UNSUPPORTED) report("unsupported", "this phone has no Wi-Fi Direct");
            if (what.endsWith("group")) pendingUntil = 0;
            // Clearing what is not there may fail: carry on with the step after it.
            if (what.startsWith("clear") && next != null && running) main.post(() -> { try { next.run(); } catch (SecurityException ignored) { } });
        }
    }
}
