package app.sol;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.net.ConnectivityManager;
import android.net.Network;
import android.os.Build;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.util.ArrayList;
import java.util.List;

/**
 * Runs this device's Sol node (libsol.so, the same `sol` binary as on a desktop) in a foreground
 * service so Android keeps it alive in the background, and restarts it if it dies.
 *
 * First run: `libsol.so init --name N --files <filesDir>/SolDrive --ui 127.0.0.1:7777` (the default
 * transports: WireGuard, TLS direct and Tor, with the APK's own tor), then every start
 * `libsol.so run --dir <filesDir>/sol`. The service is the only thing that talks to the node
 * binary; the UI is the WebView on http://localhost:7777/. Nothing secret is logged: the node's
 * output is passed to logcat as is and it never prints keys.
 */
public class NodeService extends Service {
    /** Broadcast (limited to this app) sent when the node says it is serving the UI. */
    public static final String ACTION_READY = "app.sol.READY";
    /** Stops the node and the service (the notification's Stop button). */
    static final String ACTION_STOP = "app.sol.STOP";
    /** Re-reads the join state and redraws the notification (sent once a join link is redeemed). */
    static final String ACTION_JOINED = "app.sol.JOINED";
    /** String extra of the start intent: the person's name, needed only on the first run. */
    static final String EXTRA_NAME = "name";
    /** The UI port the node is told to serve on. */
    static final int PORT = 7777;
    /** String extra: "join" makes the first run `init --join` (a device of an existing person). */
    static final String EXTRA_MODE = "mode";
    /** String extra: this device's name for a join (default: the phone model). */
    static final String EXTRA_DEVICE = "device";

    private static final String TAG = "Sol";
    private static final String CHANNEL = "node";
    private static final int NOTE_ID = 1;
    private static volatile boolean ready;
    /** True while a node process (init or run) of this service is alive; the wipe waits on it. */
    private static volatile boolean alive;
    private volatile long lastNetKick;

    private final Handler main = new Handler(Looper.getMainLooper());
    private Thread supervisor;
    private volatile Process node;
    private volatile boolean stopping;
    private volatile boolean online = true;
    private volatile int transport = -1;
    /** The default network last reported available (null until the first report). */
    private volatile Network current;
    private ConnectivityManager cm;
    private ConnectivityManager.NetworkCallback netCallback;

    /** Where the node keeps its identity and config. */
    static File dataDir(Context c) { return new File(c.getFilesDir(), "sol"); }

    /** The synced folder: Files serves it, shared files land in it. */
    static File driveDir(Context c) { return new File(c.getFilesDir(), "SolDrive"); }

    /** True once {@code sol init} has run on this device. */
    static boolean initialised(Context c) { return new File(dataDir(c), "config.json").exists(); }

    /** True while the node reports that it is serving. */
    static boolean isReady() { return ready; }

    /** True while the node process is running (even before it serves). */
    static boolean isAlive() { return alive; }

    /** Alias of {@link #isReady()} for callers that wait on it. */
    static boolean ready() { return ready; }

    @Override
    public IBinder onBind(Intent i) { return null; }

    @Override
    public int onStartCommand(Intent in, int flags, int startId) {
        if (in != null && ACTION_STOP.equals(in.getAction())) {
            stopNode();
            stopSelf();
            return START_NOT_STICKY;
        }
        if (in != null && ACTION_JOINED.equals(in.getAction())) {
            joining = false;
            note(state());
            return START_STICKY;
        }
        Notification n = notification("Starting");
        if (Build.VERSION.SDK_INT >= 29) {
            startForeground(NOTE_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTE_ID, n);
        }
        if (in != null && (in.hasExtra(EXTRA_NAME) || in.hasExtra(EXTRA_MODE))) {
            getSharedPreferences("sol", MODE_PRIVATE).edit()
                    .putString(EXTRA_NAME, in.getStringExtra(EXTRA_NAME))
                    .putString(EXTRA_MODE, in.getStringExtra(EXTRA_MODE))
                    .putString(EXTRA_DEVICE, in.getStringExtra(EXTRA_DEVICE)).apply();
        }
        watchNetwork();
        synchronized (this) {
            if (supervisor == null || !supervisor.isAlive()) {
                stopping = false;
                supervisor = new Thread(this::supervise, "sol-supervisor");
                supervisor.start();
            }
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        stopNode();
        if (cm != null && netCallback != null) {
            try { cm.unregisterNetworkCallback(netCallback); } catch (RuntimeException ignored) { }
        }
        super.onDestroy();
    }

    private void stopNode() {
        stopping = true;
        ready = false;
        Process p = node;
        if (p != null) p.destroy(); // SIGTERM: the node closes cleanly
    }

    /** Runs init once, then keeps `run` alive with a growing delay between crashes. */
    private void supervise() {
        if (!initialised(this)) {
            SharedPreferences sp = getSharedPreferences("sol", MODE_PRIVATE);
            String name = sp.getString(EXTRA_NAME, "");
            boolean join = "join".equals(sp.getString(EXTRA_MODE, ""));
            if (name == null || (name.isEmpty() && !join)) { // nothing to init with: the activity asks first
                note("Waiting for setup");
                return;
            }
            String device = sp.getString(EXTRA_DEVICE, "");
            if (device == null || device.isEmpty()) device = Build.MODEL;
            driveDir(this).mkdirs();
            int rc = join
                    ? runOnce("init", "--dir", dataDir(this).getAbsolutePath(), "--join", "--device", device,
                            "--files", driveDir(this).getAbsolutePath(), "--ui", "127.0.0.1:" + PORT)
                    : runOnce("init", "--dir", dataDir(this).getAbsolutePath(), "--name", name,
                            "--device", device, "--files", driveDir(this).getAbsolutePath(),
                            "--ui", "127.0.0.1:" + PORT);
            if (rc != 0 || !initialised(this)) {
                note("Setup failed");
                return;
            }
        }
        long delay = 1000;
        while (!stopping) {
            long start = System.currentTimeMillis();
            note("Starting");
            runOnce("run", "--dir", dataDir(this).getAbsolutePath());
            ready = false;
            if (stopping) break;
            // A run that lasted a while was healthy: start over from a short delay.
            if (System.currentTimeMillis() - start > 60_000) delay = 1000;
            note("Restarting");
            try { Thread.sleep(delay); } catch (InterruptedException e) { return; }
            delay = Math.min(delay * 2, 60_000);
        }
    }

    /** Runs the node binary to completion, forwarding its output to logcat; returns the exit code. */
    private int runOnce(String... args) {
        List<String> cmd = new ArrayList<>();
        cmd.add(getApplicationInfo().nativeLibraryDir + "/libsol.so");
        for (String a : args) cmd.add(a);
        ProcessBuilder pb = new ProcessBuilder(cmd).redirectErrorStream(true);
        env(this, pb);
        try {
            Process p = pb.start();
            node = p;
            alive = true;
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.i(TAG, args[0] + ": " + line);
                    if (args[0].equals("run") && line.startsWith("Sol is running")) {
                        ready = true;
                        // A device that has not been joined yet has no person name: say so
                        // rather than printing "Sol is running for  on <device>".
                        joining = line.startsWith("Sol is running for  on ");
                        note(state());
                        sendBroadcast(new Intent(ACTION_READY).setPackage(getPackageName()));
                    }
                }
            }
            return p.waitFor();
        } catch (IOException e) {
            Log.e(TAG, "cannot start node: " + e.getMessage());
            return -1;
        } catch (InterruptedException e) {
            return -1;
        } finally {
            alive = false;
        }
    }

    private static volatile boolean joining;

    /**
     * The node's environment. Go on Android has no sensible HOME or TMPDIR, so they point at app
     * storage. SOL_TOR_BIN is the Tor shipped in the APK (lib/<abi>/libtor.so, unpacked into the
     * native library directory, the only place an app may execute files from), which the node
     * runs for its Tor transport instead of looking for a tor on PATH.
     */
    private static void env(Context c, ProcessBuilder pb) {
        pb.environment().put("HOME", c.getFilesDir().getAbsolutePath());
        pb.environment().put("TMPDIR", c.getCacheDir().getAbsolutePath());
        File tor = new File(c.getApplicationInfo().nativeLibraryDir, "libtor.so");
        if (tor.exists()) pb.environment().put("SOL_TOR_BIN", tor.getAbsolutePath());
    }

    /** The notification text for the node's current state. */
    private String state() {
        if (joining) return "Joining\u2026";
        return online ? "Running" : "Running, no network";
    }

    /** Tells the node the network changed so it redials now (`libsol.so network-changed`). */
    private void kickNode(String reason) {
        long now = System.currentTimeMillis();
        if (!ready || now - lastNetKick < 1500) return; // Wi-Fi/cellular flaps come in bursts
        lastNetKick = now;
        new Thread(() -> cli(this, "network-changed", reason), "sol-netkick").start();
    }

    /**
     * Runs a short `libsol.so` command (such as `pair <link>`) against this device's node and
     * returns its exit code and combined output, which is user-facing text. The link is passed as
     * an argument and never logged.
     */
    static String[] cli(Context c, String... args) {
        List<String> cmd = new ArrayList<>();
        cmd.add(c.getApplicationInfo().nativeLibraryDir + "/libsol.so");
        cmd.add(args[0]);
        cmd.add("--dir");
        cmd.add(dataDir(c).getAbsolutePath());
        for (int i = 1; i < args.length; i++) cmd.add(args[i]);
        ProcessBuilder pb = new ProcessBuilder(cmd).redirectErrorStream(true);
        env(c, pb);
        try {
            Process p = pb.start();
            StringBuilder out = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) out.append(line).append('\n');
            }
            return new String[]{String.valueOf(p.waitFor()), out.toString().trim()};
        } catch (IOException | InterruptedException e) {
            return new String[]{"-1", "cannot run the node: " + e.getMessage()};
        }
    }

    /**
     * Tracks the default network: the notification says whether there is one, and a new default
     * network (Wi-Fi back, Wi-Fi to cellular) runs `libsol.so network-changed` so the node redials
     * and restarts Tor at once, instead of waiting for its own netlink watch, which Android's
     * SELinux policy denies to apps.
     */
    private void watchNetwork() {
        if (netCallback != null) return;
        cm = getSystemService(ConnectivityManager.class);
        netCallback = new ConnectivityManager.NetworkCallback() {
            @Override public void onAvailable(Network n) {
                online = true;
                // The first call reports the network the node started on: nothing changed.
                boolean changed = current != null && !n.equals(current);
                current = n;
                if (ready) { note(state()); if (changed) kickNode("available"); }
            }
            @Override public void onLost(Network n) { online = false; if (ready) note(state()); }
            @Override public void onCapabilitiesChanged(Network n, android.net.NetworkCapabilities c) {
                // Wi-Fi <-> cellular: the default network's transport changed.
                int t = c.hasTransport(android.net.NetworkCapabilities.TRANSPORT_WIFI) ? 1
                        : c.hasTransport(android.net.NetworkCapabilities.TRANSPORT_CELLULAR) ? 2 : 0;
                if (t != transport) { boolean first = transport == -1; transport = t; if (!first) kickNode(t == 1 ? "wifi" : t == 2 ? "cellular" : "other"); }
            }
        };
        try { cm.registerDefaultNetworkCallback(netCallback, main); } catch (RuntimeException e) { netCallback = null; }
    }

    private void note(String text) {
        getSystemService(NotificationManager.class).notify(NOTE_ID, notification(text));
    }

    private Notification notification(String text) {
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, getString(R.string.notif_channel), NotificationManager.IMPORTANCE_LOW));
        PendingIntent open = PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        PendingIntent stop = PendingIntent.getService(this, 1, new Intent(this, NodeService.class).setAction(ACTION_STOP), PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle("Sol")
                .setContentText(text)
                .setContentIntent(open)
                .setOngoing(true)
                .addAction(new Notification.Action.Builder(null, "Stop", stop).build())
                .build();
    }
}
