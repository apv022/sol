package app.sol;

import android.Manifest;
import android.accounts.AbstractAccountAuthenticator;
import android.accounts.Account;
import android.accounts.AccountAuthenticatorResponse;
import android.accounts.AccountManager;
import android.app.Service;
import android.content.AbstractThreadedSyncAdapter;
import android.content.ContentProviderClient;
import android.content.ContentResolver;
import android.content.Context;
import android.content.Intent;
import android.content.SyncResult;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.provider.CalendarContract;
import android.util.Log;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;

/**
 * Sol calendars in the phone's calendar apps (Google Calendar, Etar, Fossify, widgets: anything
 * that reads Android's CalendarProvider), and edits made there back into Sol.
 *
 * AOSP framework only: no Google account, no Play services, no push service. A local account of
 * type "app.sol" (named after the person, no password, nothing stored in it) owns one calendar
 * row per Sol calendar; a sync adapter bound to com.android.calendar (CalendarSyncer) writes
 * Sol's events into it as the sync adapter and sends rows edited in another app back through the
 * Calendar app's actions (`libsol.so do calendar.*`).
 *
 * Syncs are change-driven, never on a timer: Android runs the adapter when a calendar app
 * changes a row of the account (setSyncAutomatically; the default daily periodic sync is
 * removed), and the node side asks for one when the node starts serving and when an entry or feed
 * event arrives on the Calendar app's own event stream (Watch, debounced). The stream is the
 * Calendar origin's `/_sol/v1/events`, signed in like a page of it, because the admin stream that
 * `libsol.so events` follows has no feed events.
 *
 * Off by default; the person turns it on in Sol (window.SolAndroid.calendarSync(true)), which
 * asks for READ_CALENDAR/WRITE_CALENDAR then. Turning it off removes the account, and with it
 * every calendar row it owns.
 */
final class CalendarSync {
    private CalendarSync() { }

    private static final String TAG = "Sol";
    /** The account type (res/xml/authenticator.xml, res/xml/syncadapter.xml). */
    static final String ACCOUNT_TYPE = "app.sol";
    static final String AUTHORITY = CalendarContract.AUTHORITY;
    static final String[] PERMISSIONS = {Manifest.permission.READ_CALENDAR, Manifest.permission.WRITE_CALENDAR};
    private static final String PREFS = "calendar";
    private static final String PREF_ON = "on";
    /** The Calendar origin's sol_app cookie for the change stream (a secret: 0600, never logged). */
    private static final String SESSION_FILE = "calendar-session";

    private static final Handler main = new Handler(Looper.getMainLooper());
    private static final Runnable debounced = new Runnable() {
        @Override public void run() { if (appCtx != null) requestSync(appCtx); }
    };
    private static volatile Context appCtx;

    /** True when the person turned sync on and the permissions are granted (the account may still be on its way). */
    static boolean wanted(Context c) {
        return c.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(PREF_ON, false) && permitted(c);
    }

    /** True when sync is on and Android has the account. */
    static boolean enabled(Context c) { return wanted(c) && account(c) != null; }

    /** Whether both calendar permissions are granted. */
    static boolean permitted(Context c) {
        for (String p : PERMISSIONS) if (c.checkSelfPermission(p) != PackageManager.PERMISSION_GRANTED) return false;
        return true;
    }

    /** This app's account, or null. */
    static Account account(Context c) {
        Account[] a = AccountManager.get(c).getAccountsByType(ACCOUNT_TYPE);
        return a.length > 0 ? a[0] : null;
    }

    /**
     * Turns sync on (call off the main thread, with the permissions granted). The account is
     * named after the person, which only the node knows, so it is added now if the node serves,
     * else as soon as it does (onNodeReady). False when the permissions are missing.
     */
    static boolean enable(Context c) {
        if (!permitted(c)) return false;
        c.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean(PREF_ON, true).apply();
        if (NodeService.isReady()) onNodeReady(c);
        return true;
    }

    /** Adds the account if it is missing and makes it syncable for calendars, on changes only. */
    private static Account ensureAccount(Context c) {
        Account acc = account(c);
        if (acc == null) {
            acc = new Account(personName(c), ACCOUNT_TYPE);
            if (!AccountManager.get(c).addAccountExplicitly(acc, null, null)) {
                Log.w(TAG, "calendar sync: cannot add the account");
                return null;
            }
        }
        ContentResolver.setIsSyncable(acc, AUTHORITY, 1);
        ContentResolver.setSyncAutomatically(acc, AUTHORITY, true);
        // Android adds a daily periodic sync to every syncable adapter; Sol syncs on changes only.
        ContentResolver.removePeriodicSync(acc, AUTHORITY, Bundle.EMPTY);
        return acc;
    }

    /** Turns sync off (off the main thread): removes the calendar rows, the account and the stream session. */
    static void disable(Context c) {
        c.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean(PREF_ON, false).apply();
        Watch.stop();
        unobserve(c.getApplicationContext());
        Account acc = account(c);
        if (acc != null) {
            if (permitted(c)) {
                try {
                    // The provider drops an account's rows when the account goes; delete them
                    // first anyway, so nothing is left if removal is slow or refused.
                    c.getContentResolver().delete(CalendarSyncer.asAdapter(CalendarContract.Calendars.CONTENT_URI, acc),
                            CalendarContract.Calendars.ACCOUNT_TYPE + "=?", new String[]{ACCOUNT_TYPE});
                } catch (RuntimeException e) {
                    Log.w(TAG, "calendar sync: cannot delete the calendars: " + e.getClass().getSimpleName());
                }
            }
            AccountManager.get(c).removeAccountExplicitly(acc);
        }
        new File(c.getFilesDir(), SESSION_FILE).delete();
    }

    /** Asks Android to run the adapter now (ignored when sync is off). */
    static void requestSync(Context c) {
        if (!enabled(c)) return;
        Bundle b = new Bundle();
        b.putBoolean(ContentResolver.SYNC_EXTRAS_MANUAL, true); // the person turned it on in Sol
        b.putBoolean(ContentResolver.SYNC_EXTRAS_EXPEDITED, true);
        ContentResolver.requestSync(account(c), AUTHORITY, b);
    }

    /** A sync soon, coalescing bursts (a feed catching up sends many entries). */
    static void requestSyncSoon(Context c) {
        appCtx = c.getApplicationContext();
        main.removeCallbacks(debounced);
        main.postDelayed(debounced, 1500);
    }

    /** The node started serving: catch up and follow the Calendar app's changes and the provider's. */
    static synchronized void onNodeReady(Context c) {
        if (!wanted(c) || ensureAccount(c) == null) return;
        requestSync(c);
        Watch.start(c);
        main.post(() -> observe(c.getApplicationContext()));
    }

    private static android.database.ContentObserver observer;
    private static final Runnable localCheck = () -> {
        Context c = appCtx;
        if (c != null) new Thread(() -> { if (hasLocalChanges(c)) requestSync(c); }, "sol-calendar-local").start();
    };

    /**
     * While the node runs, a change in the calendar provider that left a row of the account
     * dirty (an edit in a calendar app) syncs now. Android would sync it anyway (the adapter
     * syncs automatically), but JobScheduler may hold such a local-change sync for minutes; an
     * expedited request goes at once. The adapter's own writes leave nothing dirty, so they end
     * here without a sync.
     */
    private static void observe(Context c) {
        if (observer != null || !enabled(c)) return;
        appCtx = c;
        observer = new android.database.ContentObserver(main) {
            @Override public void onChange(boolean self) {
                main.removeCallbacks(localCheck);
                main.postDelayed(localCheck, 1000);
            }
        };
        c.getContentResolver().registerContentObserver(CalendarContract.CONTENT_URI, true, observer);
    }

    private static void unobserve(Context c) {
        main.post(() -> {
            if (observer == null) return;
            c.getContentResolver().unregisterContentObserver(observer);
            observer = null;
        });
    }

    /** Whether a row of the account waits to be sent to Sol. */
    private static boolean hasLocalChanges(Context c) {
        if (!enabled(c)) return false;
        Account acc = account(c);
        if (acc == null) return false;
        try (android.database.Cursor cur = c.getContentResolver().query(CalendarSyncer.asAdapter(CalendarContract.Events.CONTENT_URI, acc),
                new String[]{CalendarContract.Events._ID},
                CalendarContract.Events.ACCOUNT_TYPE + "=? AND (" + CalendarContract.Events.DIRTY + "=1 OR "
                        + CalendarContract.Events.DELETED + "=1)", new String[]{ACCOUNT_TYPE}, null)) {
            return cur != null && cur.getCount() > 0;
        } catch (RuntimeException e) {
            return false;
        }
    }

    /** The node stopped (the service ends): stop following changes. */
    static void nodeStopped(Context c) {
        Watch.stop();
        unobserve(c.getApplicationContext());
    }

    /** Apps changed (the Calendar app installed, updated or removed): follow it again. */
    static void onAppsChanged(Context c) {
        if (!enabled(c) || !NodeService.isReady()) return;
        Watch.start(c);
        requestSyncSoon(c);
    }

    /** The person's name for the account (shown in Settings > Accounts and in calendar apps). */
    private static String personName(Context c) {
        String[] r = NodeService.cli(c, "status", "--json");
        if ("0".equals(r[0])) {
            try {
                String n = new JSONObject(r[1]).getJSONObject("me").getJSONObject("person").optString("name", "").trim();
                if (!n.isEmpty()) return n;
            } catch (org.json.JSONException ignored) { }
        }
        return "Sol";
    }

    // ---- the Calendar app's change stream ----

    /**
     * Follows `/_sol/v1/events` on the Calendar origin while the node serves and sync is on; every
     * entry or feed event asks for a sync (debounced). The origin is signed in with its own
     * sol_app cookie, made from a one-time code (`libsol.so open --print`) exactly as the
     * WebView does, kept in files/calendar-session (0600) so restarts do not pile up sessions.
     * A dropped stream is reconnected with a growing delay while the node serves; a node with no
     * Calendar app ends it (onAppsChanged starts it again).
     */
    static final class Watch {
        private static Thread thread;
        private static volatile boolean stopped;
        private static volatile HttpURLConnection conn;

        static synchronized void start(Context c) {
            stopped = false;
            if (thread != null && thread.isAlive()) return;
            Context app = c.getApplicationContext();
            thread = new Thread(() -> loop(app), "sol-calendar-watch");
            thread.start();
        }

        static synchronized void stop() {
            stopped = true;
            HttpURLConnection h = conn;
            if (h != null) h.disconnect();
            if (thread != null) thread.interrupt();
        }

        private static void loop(Context c) {
            long delay = 1000;
            boolean fresh = false; // a new cookie was made in this round
            while (!stopped && NodeService.isReady() && enabled(c)) {
                String cookie = readSession(c);
                if (cookie == null) { cookie = newSession(c); fresh = true; }
                int code = cookie == null ? -1 : follow(c, cookie);
                if (stopped) return;
                if (code == 404) { Log.i(TAG, "calendar sync: no Calendar app on this node"); return; }
                if (code == 401 || code == 403) {
                    new File(c.getFilesDir(), SESSION_FILE).delete();
                    if (!fresh) continue; // a stale cookie: make one now
                }
                if (code == 200) delay = 1000; // it was up: start the backoff over
                fresh = false;
                try { Thread.sleep(delay); } catch (InterruptedException e) { if (stopped) return; }
                delay = Math.min(delay * 2, 30_000);
            }
        }

        /** Reads the stream until it ends; returns the HTTP status (200 after a normal end), -1 on I/O errors. */
        private static int follow(Context c, String cookie) {
            try {
                HttpURLConnection h = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT + "/_sol/v1/events").openConnection();
                conn = h;
                h.setInstanceFollowRedirects(false);
                h.setRequestProperty("Host", "calendar." + SolWeb.ORIGIN_SUFFIX + ":" + NodeService.PORT);
                h.setRequestProperty("Accept", "text/event-stream");
                h.setRequestProperty("Cookie", cookie);
                h.setReadTimeout(0);
                try {
                    int code = h.getResponseCode();
                    if (code != 200) return code;
                    requestSyncSoon(c); // anything missed while the stream was down
                    try (BufferedReader r = new BufferedReader(new InputStreamReader(h.getInputStream(), StandardCharsets.UTF_8))) {
                        String line;
                        while ((line = r.readLine()) != null) {
                            if (line.equals("event: entry") || line.equals("event: feed")) requestSyncSoon(c);
                        }
                    }
                    return 200;
                } finally {
                    h.disconnect();
                    conn = null;
                }
            } catch (IOException e) {
                return -1;
            }
        }

        private static String readSession(Context c) {
            File f = new File(c.getFilesDir(), SESSION_FILE);
            if (!f.exists()) return null;
            try {
                String s = new String(java.nio.file.Files.readAllBytes(f.toPath()), StandardCharsets.UTF_8).trim();
                return s.startsWith("sol_app=") ? s : null;
            } catch (IOException e) {
                return null;
            }
        }

        /**
         * A new sol_app cookie for the Calendar origin: a one-time code signs in a root session
         * (/login?code=), which mints an app code (/_sol/open?to=calendar), redeemed on the
         * Calendar origin (/_sol/login). Codes and cookies are never logged.
         */
        private static String newSession(Context c) {
            String[] r = NodeService.cli(c, "open", "--print");
            if (!"0".equals(r[0])) return null;
            String last = r[1].contains("\n") ? r[1].substring(r[1].lastIndexOf('\n') + 1) : r[1];
            Uri login = Uri.parse(last.trim());
            if (!"/login".equals(login.getPath()) || login.getQueryParameter("code") == null) return null;
            try {
                String[] a = get("localhost", "/login?" + login.getEncodedQuery(), null, "sol_session=");
                if (a[1] == null) return null;
                String[] b = get("localhost", "/_sol/open?to=calendar&next=%2F", a[1], null);
                Uri app = b[0] == null ? null : Uri.parse(b[0]);
                if (app == null || !("calendar." + SolWeb.ORIGIN_SUFFIX).equals(app.getHost()) || !"/_sol/login".equals(app.getPath())) return null;
                String[] d = get(app.getHost(), "/_sol/login?" + app.getEncodedQuery(), null, "sol_app=");
                if (d[1] == null) return null;
                writeSecret(new File(c.getFilesDir(), SESSION_FILE), d[1]);
                return d[1];
            } catch (IOException e) {
                return null;
            }
        }

        /** One GET to the node on loopback for host, no redirects: {Location, the cookie named by want}. */
        private static String[] get(String host, String pathQuery, String cookie, String want) throws IOException {
            HttpURLConnection h = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT + pathQuery).openConnection();
            h.setInstanceFollowRedirects(false);
            h.setRequestProperty("Host", host + ":" + NodeService.PORT);
            h.setRequestProperty("Sec-Fetch-Mode", "navigate");
            // The root session shows in Settings > Sessions under this name.
            h.setRequestProperty("User-Agent", "Calendar sync on Android");
            if (cookie != null) h.setRequestProperty("Cookie", cookie);
            try {
                h.getResponseCode();
                String got = null;
                List<String> set = h.getHeaderFields().get("Set-Cookie");
                if (set != null && want != null) for (String s : set) {
                    String kv = s.split(";", 2)[0].trim();
                    if (kv.startsWith(want) && kv.length() > want.length()) got = kv;
                }
                return new String[]{h.getHeaderField("Location"), got};
            } finally {
                h.disconnect();
            }
        }
    }

    /** Writes a secret file atomically, readable by this app only (0600). */
    static void writeSecret(File f, String s) throws IOException {
        File tmp = new File(f.getPath() + ".tmp");
        try (OutputStream o = new FileOutputStream(tmp)) {
            tmp.setReadable(false, false);
            tmp.setReadable(true, true);
            tmp.setWritable(false, false);
            tmp.setWritable(true, true);
            o.write(s.getBytes(StandardCharsets.UTF_8));
            ((FileOutputStream) o).getFD().sync();
        }
        if (!tmp.renameTo(f)) { tmp.delete(); throw new IOException("rename failed"); }
    }

    // ---- the framework's services ----

    /** The account's authenticator: a local account with no credentials, added only from Sol. */
    static final class Authenticator extends AbstractAccountAuthenticator {
        private final Context ctx;
        Authenticator(Context c) { super(c); ctx = c; }

        /** "Add account" in Android's settings: the account is added by turning sync on in Sol. */
        @Override public Bundle addAccount(AccountAuthenticatorResponse r, String type, String token, String[] features, Bundle o) {
            Bundle b = new Bundle();
            b.putInt(AccountManager.KEY_ERROR_CODE, AccountManager.ERROR_CODE_UNSUPPORTED_OPERATION);
            b.putString(AccountManager.KEY_ERROR_MESSAGE, "Turn on \"Show in phone's calendar apps\" in Sol's Calendar");
            return b;
        }
        @Override public Bundle editProperties(AccountAuthenticatorResponse r, String type) { throw new UnsupportedOperationException(); }
        @Override public Bundle confirmCredentials(AccountAuthenticatorResponse r, Account a, Bundle o) { return null; }
        @Override public Bundle getAuthToken(AccountAuthenticatorResponse r, Account a, String type, Bundle o) {
            Bundle b = new Bundle();
            b.putInt(AccountManager.KEY_ERROR_CODE, AccountManager.ERROR_CODE_UNSUPPORTED_OPERATION);
            b.putString(AccountManager.KEY_ERROR_MESSAGE, "Sol has no tokens");
            return b;
        }
        @Override public String getAuthTokenLabel(String type) { return null; }
        @Override public Bundle updateCredentials(AccountAuthenticatorResponse r, Account a, String type, Bundle o) { return null; }
        @Override public Bundle hasFeatures(AccountAuthenticatorResponse r, Account a, String[] f) {
            Bundle b = new Bundle();
            b.putBoolean(AccountManager.KEY_BOOLEAN_RESULT, false);
            return b;
        }
        /** Removing the account in Android's settings is allowed; it turns sync off. */
        @Override public Bundle getAccountRemovalAllowed(AccountAuthenticatorResponse r, Account a) {
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean(PREF_ON, false).apply();
            Watch.stop();
            Bundle b = new Bundle();
            b.putBoolean(AccountManager.KEY_BOOLEAN_RESULT, true);
            return b;
        }
    }

    /** Binds the authenticator (android.accounts.AccountAuthenticator). */
    public static final class AuthService extends Service {
        private Authenticator auth;
        @Override public void onCreate() { auth = new Authenticator(this); }
        @Override public IBinder onBind(Intent i) { return auth.getIBinder(); }
    }

    /** Binds the sync adapter (android.content.SyncAdapter). */
    public static final class SyncService extends Service {
        private static Adapter adapter;
        @Override public void onCreate() {
            synchronized (SyncService.class) { if (adapter == null) adapter = new Adapter(getApplicationContext()); }
        }
        @Override public IBinder onBind(Intent i) { return adapter.getSyncAdapterBinder(); }
    }

    /** Runs CalendarSyncer when Android asks. */
    static final class Adapter extends AbstractThreadedSyncAdapter {
        Adapter(Context c) { super(c, true, false); }

        @Override
        public void onPerformSync(Account acc, Bundle extras, String authority, ContentProviderClient provider, SyncResult result) {
            Context c = getContext();
            if (!enabled(c)) { ContentResolver.setIsSyncable(acc, authority, 0); return; }
            // The node is the source of truth; with it down there is nothing to sync against.
            // NodeService asks again once it serves, so no retry is scheduled here.
            if (!NodeService.isReady()) return;
            try {
                new CalendarSyncer(c, acc, provider, result).run();
            } catch (Exception e) {
                // Class only: messages may quote event text.
                Log.w(TAG, "calendar sync failed: " + e.getClass().getSimpleName());
                result.stats.numIoExceptions++;
            }
        }
    }

    // ---- the colours ----

    private static final String[][] COLORS = {
            {"red", "FF3B30"}, {"orange", "FF9500"}, {"yellow", "FFCC00"}, {"green", "34C759"},
            {"teal", "1AA0AC"}, {"blue", "007AFF"}, {"indigo", "4C67C1"}, {"purple", "7F5A97"},
            {"pink", "BF4B64"}, {"brown", "B38D4D"}, {"gray", "8E8E93"},
    };

    /** The ARGB colour of a Calendar palette name (ui/kit tokens, as the Calendar UI mixes them); blue for unknown. */
    static int color(String name) {
        for (String[] c : COLORS) if (c[0].equals(name)) return 0xFF000000 | Integer.parseInt(c[1], 16);
        return 0xFF007AFF;
    }

    /** Runs the CLI with JSON on stdin (`do <action> --args - --json`), so event text never reaches argv. */
    static String[] doAction(Context c, String action, JSONObject args) {
        List<String> cmd = new ArrayList<>();
        cmd.add(c.getApplicationInfo().nativeLibraryDir + "/libsol.so");
        cmd.add("do");
        cmd.add("--dir");
        cmd.add(NodeService.dataDir(c).getAbsolutePath());
        cmd.add(action);
        cmd.add("--args");
        cmd.add("-");
        cmd.add("--json");
        ProcessBuilder pb = new ProcessBuilder(cmd).redirectErrorStream(true);
        NodeService.env(c, pb);
        try {
            Process p = pb.start();
            try (OutputStream o = p.getOutputStream()) { o.write(args.toString().getBytes(StandardCharsets.UTF_8)); }
            StringBuilder out = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                char[] buf = new char[8192];
                int n;
                while ((n = r.read(buf)) > 0) out.append(buf, 0, n);
            }
            return new String[]{String.valueOf(p.waitFor()), out.toString().trim()};
        } catch (IOException | InterruptedException e) {
            return new String[]{"-1", ""};
        }
    }
}
