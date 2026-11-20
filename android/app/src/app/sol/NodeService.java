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
 * First run: `libsol.so init --name N --files <filesDir>/SolDrive --ui 127.0.0.1:7777`, then every
 * start `libsol.so run --dir <filesDir>/sol`. The service is the only thing that talks to the node
 * binary; the UI is the WebView on http://localhost:7777/. Nothing secret is logged: the node's
 * output is passed to logcat as is and it never prints keys.
 */
public class NodeService extends Service {
    /** Broadcast (limited to this app) sent when the node says it is serving the UI. */
    public static final String ACTION_READY = "app.sol.READY";
    /** Stops the node and the service (the notification's Stop button). */
    static final String ACTION_STOP = "app.sol.STOP";
    /** String extra of the start intent: the person's name, needed only on the first run. */
    static final String EXTRA_NAME = "name";
    /** The UI port the node is told to serve on. */
    static final int PORT = 7777;

    private static final String TAG = "Sol";
    private static final String CHANNEL = "node";
    private static final int NOTE_ID = 1;
    private static volatile boolean ready;

    private final Handler main = new Handler(Looper.getMainLooper());
    private Thread supervisor;
    private volatile Process node;
    private volatile boolean stopping;
    private volatile boolean online = true;
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

    @Override
    public IBinder onBind(Intent i) { return null; }

    @Override
    public int onStartCommand(Intent in, int flags, int startId) {
        if (in != null && ACTION_STOP.equals(in.getAction())) {
            stopNode();
            stopSelf();
            return START_NOT_STICKY;
        }
        Notification n = notification("Starting");
        if (Build.VERSION.SDK_INT >= 29) {
            startForeground(NOTE_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTE_ID, n);
        }
        if (in != null && in.hasExtra(EXTRA_NAME)) {
            getSharedPreferences("sol", MODE_PRIVATE).edit().putString(EXTRA_NAME, in.getStringExtra(EXTRA_NAME)).apply();
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
            String name = getSharedPreferences("sol", MODE_PRIVATE).getString(EXTRA_NAME, "");
            if (name.isEmpty()) { // nothing to init with: the activity asks for a name first
                note("Waiting for a name");
                return;
            }
            driveDir(this).mkdirs();
            int rc = runOnce("init", "--dir", dataDir(this).getAbsolutePath(), "--name", name,
                    "--device", Build.MODEL, "--files", driveDir(this).getAbsolutePath(),
                    "--ui", "127.0.0.1:" + PORT, "--transport", "direct");
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
        // Go on Android has no sensible HOME or TMPDIR; point them at app storage.
        pb.environment().put("HOME", getFilesDir().getAbsolutePath());
        pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
        try {
            Process p = pb.start();
            node = p;
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.i(TAG, args[0] + ": " + line);
                    if (args[0].equals("run") && line.startsWith("Sol is running")) {
                        ready = true;
                        note(online ? "Running" : "Running, no network");
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
        }
    }

    /** Tracks connectivity so the notification says the truth. The node re-dials on its own. */
    private void watchNetwork() {
        if (netCallback != null) return;
        cm = getSystemService(ConnectivityManager.class);
        netCallback = new ConnectivityManager.NetworkCallback() {
            @Override public void onAvailable(Network n) { online = true; if (ready) note("Running"); }
            @Override public void onLost(Network n) { online = false; if (ready) note("Running, no network"); }
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
