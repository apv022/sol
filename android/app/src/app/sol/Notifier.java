package app.sol;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.service.notification.StatusBarNotification;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * Inbox items as Android notifications, made on the phone from what this phone's node says: no
 * push service of any kind (no Firebase, no Google Play services). Something that happened while
 * the phone was offline is notified once the node reconnects and syncs it.
 *
 * NodeService runs `libsol.so events --json` (the admin event stream) as a child while the node
 * serves, restarting it when it ends. An "inbox" event carries the item: an open one is notified
 * once (the ids already notified are kept, capped, so nothing notifies twice, not even after a
 * restart); one answered or dismissed anywhere (another device, the web UI, the CLI) cancels its
 * notification. Each time the node comes up the open items are read with `inbox --json` as well,
 * so items from while the stream was down are not missed. An "app" event refreshes the launcher
 * shortcuts; a "nearby" event tells WifiDirect whether its switch is on.
 *
 * Each app has its own channel (named after the app, created when first needed), so the person
 * turns an app's notifications off in Android's own settings; Sol's own items and those of apps
 * without a UI go to "Sol". Approvals get Approve and Deny, notices Dismiss (InboxReceiver).
 * Tapping an approval opens Sol's Inbox; tapping a notice opens its app in the app's own task.
 */
final class Notifier {
    private static final String TAG = "Sol";
    /** Preferences holding the notified ids. */
    static final String PREFS = "notify";
    /** The notification id of every inbox item; the tag is the item id. */
    static final int NOTE_INBOX = 100;
    private static final int MAX_NOTIFIED = 500;
    /** Open items older than this are not notified when first seen in a sync. */
    private static final long MAX_AGE_MS = 7L * 24 * 3600 * 1000;

    private final NodeService svc;
    private volatile boolean stopped;
    private volatile Process child;
    private Thread thread;

    Notifier(NodeService svc) { this.svc = svc; }

    /** Starts following the node's events (idempotent); the thread waits for the node to serve. */
    synchronized void start() {
        stopped = false;
        if (thread != null && thread.isAlive()) return;
        thread = new Thread(this::loop, "sol-events");
        thread.start();
    }

    /** Stops the events child (the service stops). */
    synchronized void stop() {
        stopped = true;
        Process p = child;
        if (p != null) p.destroy();
        if (thread != null) thread.interrupt();
    }

    private void loop() {
        long delay = 1000;
        while (!stopped) {
            if (!NodeService.isReady()) {
                try { Thread.sleep(500); } catch (InterruptedException e) { if (stopped) return; }
                continue;
            }
            long start = System.currentTimeMillis();
            follow();
            if (stopped) return;
            if (System.currentTimeMillis() - start > 60_000) delay = 1000;
            try { Thread.sleep(delay); } catch (InterruptedException e) { if (stopped) return; }
            delay = Math.min(delay * 2, 30_000);
        }
    }

    /** Runs one `libsol.so events --json` child until it exits. */
    private void follow() {
        List<String> cmd = new ArrayList<>(Arrays.asList(svc.getApplicationInfo().nativeLibraryDir + "/libsol.so",
                "events", "--dir", NodeService.dataDir(svc).getAbsolutePath(), "--json"));
        ProcessBuilder pb = new ProcessBuilder(cmd).redirectErrorStream(true);
        NodeService.env(svc, pb);
        try {
            Process p = pb.start();
            child = p;
            // The stream starts from now: read what is already open once it is connected.
            new Thread(() -> {
                try { Thread.sleep(1000); } catch (InterruptedException ignored) { }
                sync();
            }, "sol-inbox-sync").start();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) {
                    if (!line.startsWith("{")) { Log.i(TAG, "events: " + line); continue; }
                    try { event(new JSONObject(line)); }
                    catch (JSONException | RuntimeException e) { Log.w(TAG, "events: " + e.getMessage()); }
                }
            }
            p.waitFor();
        } catch (IOException e) {
            Log.e(TAG, "cannot follow events: " + e.getMessage());
        } catch (InterruptedException ignored) {
        } finally {
            child = null;
        }
    }

    private void event(JSONObject ev) {
        String type = ev.optString("type");
        JSONObject data = ev.optJSONObject("data");
        if ("app".equals(type)) {
            Shortcuts.refreshAsync(svc);
            CalendarSync.onAppsChanged(svc);
        } else if ("inbox".equals(type) && data != null) {
            item(data, false);
        } else if ("nearby".equals(type) && data != null) {
            svc.wifiDirect.onNearby(data); // the Wi-Fi Direct switch, from any device or agent
        }
    }

    /** Brings notifications in line with `inbox --json`: notifies open items, cancels the rest. */
    void sync() {
        String[] r = NodeService.cli(svc, "inbox", "--json");
        if (!r[0].equals("0")) return;
        JSONArray a;
        try { a = new JSONArray(r[1].substring(Math.max(0, r[1].indexOf('[')))); }
        catch (JSONException e) { return; }
        Set<String> open = new HashSet<>();
        for (int i = 0; i < a.length(); i++) {
            JSONObject it = a.optJSONObject(i);
            if (it == null) continue;
            if (isOpen(it)) open.add(it.optString("id"));
            item(it, true);
        }
        NotificationManager nm = svc.getSystemService(NotificationManager.class);
        for (StatusBarNotification s : nm.getActiveNotifications()) {
            if (s.getId() == NOTE_INBOX && s.getTag() != null && !open.contains(s.getTag())) nm.cancel(s.getTag(), NOTE_INBOX);
        }
    }

    private static boolean isOpen(JSONObject it) {
        return !it.optBoolean("removed", false) && it.optString("answer", "").isEmpty() && !it.optString("id", "").isEmpty();
    }

    /** One item from an event or a sync: notify it if open and new, else cancel its notification. */
    private void item(JSONObject it, boolean fromSync) {
        String id = it.optString("id", "");
        if (id.isEmpty()) return;
        if (!isOpen(it)) { cancel(svc, id); return; }
        if (!markNotified(id)) return;
        if (fromSync && age(it) > MAX_AGE_MS) return; // old news, not worth a buzz now
        post(svc, it, null);
    }

    private static long age(JSONObject it) {
        try {
            return System.currentTimeMillis() - java.time.OffsetDateTime.parse(it.optString("time")).toInstant().toEpochMilli();
        } catch (RuntimeException e) {
            return 0;
        }
    }

    /** Records id as notified; false if it already was. Keeps the newest MAX_NOTIFIED ids. */
    private synchronized boolean markNotified(String id) {
        SharedPreferences sp = svc.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        String raw = sp.getString("ids", "");
        List<String> ids = new ArrayList<>();
        if (!raw.isEmpty()) ids.addAll(Arrays.asList(raw.split("\n")));
        if (ids.contains(id)) return false;
        ids.add(id);
        while (ids.size() > MAX_NOTIFIED) ids.remove(0);
        sp.edit().putString("ids", String.join("\n", ids)).apply();
        return true;
    }

    static void cancel(Context c, String id) {
        c.getSystemService(NotificationManager.class).cancel(id, NOTE_INBOX);
    }

    /**
     * Posts the notification of an open item; problem, when set, replaces the body (an Approve
     * that failed says why).
     */
    static void post(Context c, JSONObject it, String problem) {
        String id = it.optString("id"), app = it.optString("app", "");
        boolean approval = "approval".equals(it.optString("kind"));
        String appName = Shortcuts.validId(app) ? Shortcuts.appName(c, app) : null;
        String channel = channel(c, appName == null ? null : app, appName);
        String body = problem != null ? problem : it.optString("body", "");
        if (body.isEmpty() && approval && !it.optString("agent", "").isEmpty()) body = "An agent is asking.";
        Intent open = approval || appName == null
                ? new Intent(c, MainActivity.class).putExtra(MainActivity.EXTRA_URL, SolWeb.HOME + "#/inbox")
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                : AppActivity.intent(c, app, appName);
        int code = id.hashCode();
        Notification.Builder b = new Notification.Builder(c, channel)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(it.optString("title", "Sol"))
                .setContentIntent(PendingIntent.getActivity(c, code, open,
                        PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT))
                .setAutoCancel(!approval)
                .setOnlyAlertOnce(true)
                .setGroup("inbox") // never bundled with the node's own ongoing notification
                .setCategory(approval ? Notification.CATEGORY_REMINDER : Notification.CATEGORY_MESSAGE);
        if (!body.isEmpty()) b.setContentText(body).setStyle(new Notification.BigTextStyle().bigText(body));
        if (appName != null) b.setSubText(appName).setLargeIcon(Shortcuts.icon(c, app, appName));
        long when = System.currentTimeMillis() - age(it);
        b.setWhen(when).setShowWhen(true);
        if (approval) {
            b.addAction(action(c, InboxReceiver.ACTION_APPROVE, "Approve", it, code));
            b.addAction(action(c, InboxReceiver.ACTION_DENY, "Deny", it, code + 1));
        } else {
            b.addAction(action(c, InboxReceiver.ACTION_DISMISS, "Dismiss", it, code + 2));
        }
        c.getSystemService(NotificationManager.class).notify(id, NOTE_INBOX, b.build());
    }

    private static Notification.Action action(Context c, String verb, String label, JSONObject it, int code) {
        Intent i = new Intent(c, InboxReceiver.class).setAction(verb)
                .putExtra(InboxReceiver.EXTRA_ITEM, it.toString());
        PendingIntent pi = PendingIntent.getBroadcast(c, code, i, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        return new Notification.Action.Builder(null, label, pi).build();
    }

    /**
     * The channel for an app (created on first use, named after it), or "Sol" for app == null.
     * The person's per-channel choices are Android's and are never overwritten here.
     */
    private static String channel(Context c, String app, String appName) {
        NotificationManager nm = c.getSystemService(NotificationManager.class);
        String id = app == null ? "sol" : "app." + app;
        if (nm.getNotificationChannel(id) == null) {
            NotificationChannel ch = new NotificationChannel(id, app == null ? "Sol" : appName, NotificationManager.IMPORTANCE_HIGH);
            ch.setDescription(app == null ? "Approvals and notices from Sol" : "Notices and approvals from " + appName);
            nm.createNotificationChannel(ch);
        }
        return id;
    }
}
