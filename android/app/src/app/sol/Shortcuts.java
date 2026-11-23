package app.sol;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ShortcutInfo;
import android.content.pm.ShortcutManager;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Paint;
import android.graphics.Typeface;
import android.graphics.drawable.Icon;
import android.net.Uri;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * Sol's apps on the phone's launcher: the dynamic shortcuts on Sol's own icon (long-press: Files,
 * Notes, Tasks, Calendar) and pinned home-screen icons ("Add to home screen"). Both open
 * ShortcutActivity with sol-app://&lt;id&gt;, which shows the app in its own task (AppActivity).
 *
 * The app list comes from `libsol.so apps --json` and is kept in preferences, so names and the
 * list are known before the node serves. An app's icon is the node's
 * http://&lt;id&gt;.localhost:7777/_sol/icon-192.png (public, a full-bleed square, so it makes a
 * good adaptive icon), cached under files/icons/; when the node does not serve one, or it is
 * unreachable, a square in a colour derived from the id with the name's first letter is drawn
 * instead. Shortcuts are only rewritten when the app list changed (the launcher rate-limits
 * updates from the background).
 */
final class Shortcuts {
    private static final String TAG = "Sol";
    private static final String PREFS = "shortcuts";
    /** The usual apps first on the long-press menu; the rest follow by name. */
    private static final List<String> FIRST = java.util.Arrays.asList("files", "notes", "tasks", "calendar");
    private static final int MENU = 4;

    private Shortcuts() { }

    /** An app id as the node uses them: lowercase letters, digits and dashes. */
    static boolean validId(String id) { return id != null && id.matches("[a-z0-9][a-z0-9-]{0,62}"); }

    /** The app id in sol-app://&lt;id&gt;, or null. */
    static String idOf(Uri u) {
        if (u == null || !"sol-app".equals(u.getScheme())) return null;
        String id = u.getHost();
        return validId(id) ? id : null;
    }

    /** What a shortcut fires: the trampoline, which opens the app's own task. */
    private static Intent shortcutIntent(Context c, String id, String name) {
        return new Intent(Intent.ACTION_VIEW, Uri.parse("sol-app://" + id))
                .setClass(c, ShortcutActivity.class).putExtra(AppActivity.EXTRA_NAME, name);
    }

    // ---- the app list ----

    /** One installed app with a UI. */
    static final class App {
        final String id, name;
        App(String id, String name) { this.id = id; this.name = name; }
    }

    /** The apps with a UI as last listed by the node (from preferences, so it works offline). */
    static List<App> apps(Context c) {
        List<App> out = new ArrayList<>();
        String raw = c.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("apps", "[]");
        try {
            JSONArray a = new JSONArray(raw);
            for (int i = 0; i < a.length(); i++) {
                JSONObject o = a.getJSONObject(i);
                String id = o.optString("id");
                if (!validId(id) || !o.optBoolean("ui", false)) continue;
                String name = o.optString("name", "").trim();
                out.add(new App(id, name.isEmpty() ? id : name));
            }
        } catch (JSONException ignored) { }
        Collections.sort(out, (x, y) -> {
            int i = FIRST.indexOf(x.id), j = FIRST.indexOf(y.id);
            if (i < 0) i = FIRST.size();
            if (j < 0) j = FIRST.size();
            return i != j ? Integer.compare(i, j) : x.name.compareToIgnoreCase(y.name);
        });
        return out;
    }

    /** The app's display name, or null when it is not an installed app with a UI. */
    static String appName(Context c, String id) {
        for (App a : apps(c)) if (a.id.equals(id)) return a.name;
        return null;
    }

    /**
     * Re-reads the app list from the node and brings the launcher's shortcuts in line with it.
     * Runs `libsol.so apps --json`, so call it off the UI thread (refreshAsync).
     */
    static void refresh(Context c) {
        String[] r = NodeService.cli(c, "apps", "--json");
        if (!r[0].equals("0")) return;
        String json = r[1].substring(Math.max(0, r[1].indexOf('[')));
        try { new JSONArray(json); } catch (JSONException e) { return; }
        SharedPreferences sp = c.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        sp.edit().putString("apps", json).apply();
        List<App> apps = apps(c);
        StringBuilder sig = new StringBuilder();
        for (App a : apps) {
            fetchIcon(c, a.id);
            sig.append(a.id).append('=').append(a.name).append('/').append(new File(iconDir(c), a.id + ".png").length()).append(';');
        }
        if (sig.toString().equals(sp.getString("sig", null))) return;
        ShortcutManager sm = c.getSystemService(ShortcutManager.class);
        if (sm == null) return;
        List<ShortcutInfo> menu = new ArrayList<>();
        Set<String> ids = new HashSet<>();
        int max = Math.min(MENU, sm.getMaxShortcutCountPerActivity());
        for (App a : apps) {
            ids.add(a.id);
            if (menu.size() < max) menu.add(info(c, a.id, a.name, menu.size()));
        }
        try {
            sm.setDynamicShortcuts(menu);
            // Pinned icons follow renames; an app that is gone greys its icon out, and comes back
            // when it is installed again.
            List<ShortcutInfo> update = new ArrayList<>();
            List<String> gone = new ArrayList<>();
            for (ShortcutInfo s : sm.getPinnedShortcuts()) {
                String id = s.getId();
                if (ids.contains(id)) update.add(info(c, id, appName(c, id), 0));
                else gone.add(id);
            }
            if (!update.isEmpty()) {
                List<String> back = new ArrayList<>();
                for (ShortcutInfo s : update) back.add(s.getId());
                sm.enableShortcuts(back);
                sm.updateShortcuts(update);
            }
            if (!gone.isEmpty()) sm.disableShortcuts(gone, "This app is no longer in Sol");
            sp.edit().putString("sig", sig.toString()).apply();
        } catch (RuntimeException e) { // IllegalStateException when rate-limited: try next time
            Log.w(TAG, "shortcuts: " + e.getMessage());
        }
    }

    /** refresh on a background thread. */
    static void refreshAsync(Context c) {
        Context app = c.getApplicationContext();
        new Thread(() -> refresh(app), "sol-shortcuts").start();
    }

    /** Removes every shortcut (Sol was removed from the phone). */
    static void clear(Context c) {
        ShortcutManager sm = c.getSystemService(ShortcutManager.class);
        if (sm == null) return;
        try {
            sm.removeAllDynamicShortcuts();
            List<String> pinned = new ArrayList<>();
            for (ShortcutInfo s : sm.getPinnedShortcuts()) pinned.add(s.getId());
            if (!pinned.isEmpty()) sm.disableShortcuts(pinned, "Set up Sol again to use this");
        } catch (RuntimeException ignored) { }
        c.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().clear().apply();
    }

    /** Deletes the cached icons. */
    static void clearIcons(Context c) {
        File[] fs = iconDir(c).listFiles();
        if (fs != null) for (File f : fs) f.delete();
    }

    private static ShortcutInfo info(Context c, String id, String name, int rank) {
        if (name == null) name = id;
        return new ShortcutInfo.Builder(c, id)
                .setShortLabel(name)
                .setLongLabel(name)
                .setIcon(Icon.createWithAdaptiveBitmap(icon(c, id, name)))
                .setIntent(shortcutIntent(c, id, name))
                .setRank(rank)
                .build();
    }

    /**
     * "Add to home screen": asks the launcher to pin the app's icon (the launcher shows its own
     * confirmation). False when the launcher cannot pin shortcuts. The icon may need the network
     * (the node on loopback), so the request is made from a background thread.
     */
    static boolean pin(Activity a, String id, String name) {
        ShortcutManager sm = a.getSystemService(ShortcutManager.class);
        if (sm == null || !sm.isRequestPinShortcutSupported()) return false;
        Context app = a.getApplicationContext();
        new Thread(() -> {
            fetchIcon(app, id);
            ShortcutInfo s = info(app, id, name, 0);
            try {
                sm.requestPinShortcut(s, null);
            } catch (RuntimeException e) {
                Log.w(TAG, "pin shortcut: " + e.getMessage());
            }
        }, "sol-pin").start();
        return true;
    }

    // ---- icons ----

    private static File iconDir(Context c) { return new File(c.getFilesDir(), "icons"); }

    /** The app's icon: the node's PNG as last fetched, else a drawn one. Never null. */
    static Bitmap icon(Context c, String id, String name) {
        File f = new File(iconDir(c), id + ".png");
        if (f.exists()) {
            Bitmap b = BitmapFactory.decodeFile(f.getPath());
            if (b != null) return b;
        }
        return fallback(id, name);
    }

    /**
     * Fetches http://&lt;id&gt;.localhost:7777/_sol/icon-192.png into the cache (replacing it
     * only with a PNG that decodes). Blocking; false when the node had none.
     */
    static boolean fetchIcon(Context c, String id) {
        if (!validId(id)) return false;
        try {
            HttpURLConnection h = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT
                    + "/_sol/icon-192.png").openConnection();
            h.setRequestProperty("Host", id + "." + SolWeb.ORIGIN_SUFFIX + ":" + NodeService.PORT);
            h.setConnectTimeout(3000);
            h.setReadTimeout(5000);
            try {
                if (h.getResponseCode() != 200) return false;
                String type = h.getContentType();
                if (type != null && !type.startsWith("image/")) return false;
                File dir = iconDir(c);
                dir.mkdirs();
                File tmp = new File(dir, id + ".png.part");
                try (InputStream in = h.getInputStream(); OutputStream out = new FileOutputStream(tmp)) {
                    byte[] buf = new byte[8192];
                    long total = 0;
                    for (int n; (n = in.read(buf)) > 0; ) {
                        total += n;
                        if (total > 1 << 20) throw new java.io.IOException("icon too large");
                        out.write(buf, 0, n);
                    }
                }
                if (BitmapFactory.decodeFile(tmp.getPath()) == null || !tmp.renameTo(new File(dir, id + ".png"))) {
                    tmp.delete();
                    return false;
                }
                return true;
            } finally {
                h.disconnect();
            }
        } catch (java.io.IOException e) {
            return false;
        }
    }

    /** A full-bleed square in a colour derived from the id, with the name's first letter. */
    static Bitmap fallback(String id, String name) {
        int size = 192;
        Bitmap b = Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888);
        Canvas cv = new Canvas(b);
        float hue = (id.hashCode() & 0x7fffffff) % 360;
        cv.drawColor(Color.HSVToColor(new float[]{hue, 0.55f, 0.72f}));
        String letter = (name == null || name.trim().isEmpty() ? id : name.trim()).substring(0, 1).toUpperCase(java.util.Locale.ROOT);
        Paint p = new Paint(Paint.ANTI_ALIAS_FLAG);
        p.setColor(Color.WHITE);
        p.setTypeface(Typeface.create(Typeface.DEFAULT, Typeface.BOLD));
        p.setTextSize(size * 0.36f); // inside the adaptive icon's safe zone
        p.setTextAlign(Paint.Align.CENTER);
        Paint.FontMetrics m = p.getFontMetrics();
        cv.drawText(letter, size / 2f, size / 2f - (m.ascent + m.descent) / 2f, p);
        return b;
    }
}
