package app.sol;

import android.app.Activity;
import android.content.ContentValues;
import android.content.Intent;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Environment;
import android.provider.MediaStore;
import android.view.View;
import android.view.WindowInsets;
import android.webkit.CookieManager;
import android.webkit.URLUtil;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.Toast;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * One activity's window onto Sol's web UIs: an edge-to-edge window and a WebView signed in to
 * the node, shared by MainActivity (the Sol app) and AppActivity (one app in its own task).
 *
 * The launcher is http://localhost:7777/ and each app is http://&lt;app&gt;.localhost:7777/
 * (Chromium resolves *.localhost to loopback itself). The page draws under transparent system
 * bars and is told their size as --sol-inset-* CSS properties (WebView does not supply them to
 * the kit as env()); only the keyboard pads it natively. Pages reach the shell through
 * window.SolAndroid (Bridge), which answers only while the top page is a local Sol origin. The
 * WebView never shows other sites: they open in the browser. CookieManager is process-wide, so
 * every window shares the one Sol session and each app origin's cookie.
 */
final class SolWeb {
    static final String ORIGIN_SUFFIX = "localhost";
    static final String HOME = "http://localhost:" + NodeService.PORT + "/";
    static final int REQ_FILE = 1;
    /** The calendar permissions request (calendarSync(true)); activities forward the result to permissionsResult. */
    static final int REQ_CALENDAR = 3;
    /** The Nearby devices (or, below Android 13, location) request (wifiDirect(true)). */
    static final int REQ_WIFI_DIRECT = 4;

    private final Activity act;
    private final FrameLayout root;
    /** Runs after the insets change (MainActivity keeps its native setup screens clear of the bars). */
    Runnable onInsets;
    /** Settings' "Remove Sol from this phone" (only the Sol app offers it); null ignores it. */
    Runnable onReset;

    WebView web;
    private ValueCallback<Uri[]> chooser;
    private String afterLogin;
    /** The URL of the top-level page, kept for the bridge (which runs off the UI thread). */
    private volatile String pageUrl;
    /** App sign-ins that failed in a row: stop sending the page round in circles. */
    private int openFailures;

    SolWeb(Activity act, FrameLayout root) {
        this.act = act;
        this.root = root;
    }

    // ---- window ----

    /** The system bar insets (status, navigation, cutout) and the keyboard's, in physical px. */
    int insTop, insBottom, insLeft, insRight, insIme;

    /**
     * Draws the whole window under transparent status and navigation bars. Nothing is padded
     * natively except by the keyboard (so a focused field stays above it): the WebView reaches the
     * screen edges and the page is told the bar insets as the CSS custom properties
     * --sol-inset-top/-bottom/-left/-right on &lt;html&gt; (pushInsets), which the kit folds into
     * --safe-*. Android's WebView does not fill env(safe-area-inset-*) with the system bars.
     */
    void edgeToEdge() {
        android.view.Window w = act.getWindow();
        if (Build.VERSION.SDK_INT >= 30) w.setDecorFitsSystemWindows(false);
        else root.setSystemUiVisibility(View.SYSTEM_UI_FLAG_LAYOUT_STABLE | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION);
        if (Build.VERSION.SDK_INT >= 29) { // no grey scrim behind the bars: the page draws there
            w.setNavigationBarContrastEnforced(false);
            w.setStatusBarContrastEnforced(false);
        }
        if (Build.VERSION.SDK_INT >= 28) {
            android.view.WindowManager.LayoutParams a = w.getAttributes();
            a.layoutInDisplayCutoutMode = android.view.WindowManager.LayoutParams.LAYOUT_IN_DISPLAY_CUTOUT_MODE_SHORT_EDGES;
            w.setAttributes(a);
        }
        barIcons();
        root.setOnApplyWindowInsetsListener((v, ins) -> {
            int top, bottom, left, right, ime;
            if (Build.VERSION.SDK_INT >= 30) {
                android.graphics.Insets b = ins.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout());
                top = b.top; bottom = b.bottom; left = b.left; right = b.right;
                ime = ins.getInsets(WindowInsets.Type.ime()).bottom;
            } else {
                top = ins.getStableInsetTop(); bottom = ins.getStableInsetBottom();
                left = ins.getStableInsetLeft(); right = ins.getStableInsetRight();
                // With adjustResize the system inset grows by the keyboard; the stable one does not.
                ime = ins.getSystemWindowInsetBottom() > bottom ? ins.getSystemWindowInsetBottom() : 0;
            }
            insTop = top; insLeft = left; insRight = right; insIme = ime;
            insBottom = ime > 0 ? 0 : bottom; // the keyboard covers the navigation bar
            v.setPadding(0, 0, 0, ime);
            if (onInsets != null) onInsets.run();
            pushInsets();
            return ins;
        });
    }

    /** Whether the system is in dark mode. */
    boolean night() {
        return (act.getResources().getConfiguration().uiMode & android.content.res.Configuration.UI_MODE_NIGHT_MASK)
                == android.content.res.Configuration.UI_MODE_NIGHT_YES;
    }

    /** Dark bar icons on a light theme, light icons on a dark one (the system's day/night mode). */
    void barIcons() {
        boolean night = night();
        if (Build.VERSION.SDK_INT >= 30) {
            android.view.WindowInsetsController c = act.getWindow().getInsetsController();
            int both = android.view.WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                    | android.view.WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
            if (c != null) c.setSystemBarsAppearance(night ? 0 : both, both);
        } else {
            int f = root.getSystemUiVisibility() & ~(View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR);
            if (!night) f |= View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            root.setSystemUiVisibility(f);
        }
    }

    /** Day/night switched: the window background and the bar icons follow. */
    void configurationChanged() {
        root.setBackgroundResource(R.color.win_bg);
        barIcons();
    }

    /**
     * Tells the current page the bar insets in CSS px: --sol-inset-* on &lt;html&gt;, which
     * ui/kit/tokens.css folds into --safe-* (max with env()). Run on every page commit and finish
     * and whenever the insets change, on every local origin (the launcher and each app).
     */
    void pushInsets() {
        if (web == null) return;
        String u = web.getUrl();
        if (u == null || !isLocal(Uri.parse(u))) return;
        float d = act.getResources().getDisplayMetrics().density;
        String js = "(function(s){s.setProperty('--sol-inset-top','" + css(insTop, d) + "');"
                + "s.setProperty('--sol-inset-bottom','" + css(insBottom, d) + "');"
                + "s.setProperty('--sol-inset-left','" + css(insLeft, d) + "');"
                + "s.setProperty('--sol-inset-right','" + css(insRight, d) + "');})(document.documentElement.style)";
        web.evaluateJavascript(js, null);
    }

    private static String css(int px, float density) {
        return String.format(java.util.Locale.ROOT, "%.1fpx", px / density);
    }

    // ---- the web view ----

    /** Creates the WebView (hidden until show) behind anything already in root. */
    void build() {
        if (web != null) return;
        web = new WebView(act);
        web.setBackgroundColor(Color.TRANSPARENT);
        web.setVisibility(View.INVISIBLE);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
        // " SolAndroid/<version>" lets the node name this session "Sol app on Android".
        s.setUserAgentString(s.getUserAgentString() + " SolAndroid/" + versionName(act));
        web.addJavascriptInterface(new Bridge(), "SolAndroid");
        CookieManager.getInstance().setAcceptCookie(true);
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest r) {
                Uri u = r.getUrl();
                if (isLocal(u) && ORIGIN_SUFFIX.equals(u.getHost()) && "/_sol/open".equals(u.getPath())) {
                    openApp(u); // an app origin asks the root to sign this WebView in there
                    return true;
                }
                if (isLocal(u)) return false; // launcher and app origins stay in this WebView
                try { act.startActivity(new Intent(Intent.ACTION_VIEW, u)); } catch (RuntimeException ignored) { }
                return true;
            }

            @Override
            public void onPageStarted(WebView v, String url, android.graphics.Bitmap icon) { pageUrl = url; }

            @Override
            public void doUpdateVisitedHistory(WebView v, String url, boolean reload) { pageUrl = url; }

            @Override
            public void onPageCommitVisible(WebView v, String url) { pushInsets(); }

            @Override
            public void onPageFinished(WebView v, String url) {
                pushInsets();
                // The login redirect has landed on /: now go where the user was headed (Files after a share).
                if (afterLogin != null && HOME.equals(url)) { String t = afterLogin; afterLogin = null; v.loadUrl(t); }
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams p) {
                if (chooser != null) chooser.onReceiveValue(null);
                chooser = cb;
                try {
                    Intent i = p.createIntent();
                    if (p.getMode() == FileChooserParams.MODE_OPEN_MULTIPLE) i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                    act.startActivityForResult(i, REQ_FILE);
                } catch (RuntimeException e) {
                    chooser = null;
                    return false;
                }
                return true;
            }
        });
        web.setDownloadListener((url, ua, cd, mime, len) -> {
            if (explicitDownload(url, cd)) download(url, cd, mime);
            else openNative(url, mime, null); // an inline preview (a PDF in an iframe): view, do not save
        });
        root.addView(web, 0, new FrameLayout.LayoutParams(-1, -1));
    }

    /** Removes and destroys the WebView (the Sol app's reset, or the activity going away). */
    void destroy() {
        if (web == null) return;
        root.removeView(web);
        web.destroy();
        web = null;
        pageUrl = null;
    }

    /** Whether the URL is this device's own UI: localhost or *.localhost on Sol's port. */
    static boolean isLocal(Uri u) {
        String h = u.getHost();
        return "http".equals(u.getScheme()) && h != null && u.getPort() == NodeService.PORT
                && (h.equals(ORIGIN_SUFFIX) || h.endsWith("." + ORIGIN_SUFFIX));
    }

    /**
     * Shows target once the node is serving. The WebView is logged into the Sol app with a
     * one-time login code from `libsol.so open --print`, which works on every Android version.
     * (The daemon's same-user auto-login reads /proc/net/tcp, which Android 10+ hides from apps,
     * so it cannot be relied on.) The session cookie outlives the process, so a restart whose
     * cookie the node still accepts just loads the page; a fresh code is fetched only when it does
     * not. An app origin is signed in from this session on first use (openApp).
     */
    void show(String target) {
        if (web == null) return;
        web.setVisibility(View.VISIBLE);
        String cookie = CookieManager.getInstance().getCookie(HOME);
        new Thread(() -> {
            if (sessionLive(cookie)) { // signed in from an earlier start: no new session
                act.runOnUiThread(() -> { if (web != null) web.loadUrl(target); });
                return;
            }
            String[] r = NodeService.cli(act, "open", "--print");
            String last = r[1].contains("\n") ? r[1].substring(r[1].lastIndexOf('\n') + 1) : r[1];
            String login = r[0].equals("0") && last.startsWith(HOME + "login?code=") ? last : null;
            act.runOnUiThread(() -> {
                if (web == null) return;
                if (login != null) { afterLogin = target.equals(HOME) ? null : target; web.loadUrl(login); }
                else web.loadUrl(target); // no code: fall back to same-user auto-login
            });
        }, "sol-login").start();
    }

    /** Whether the WebView's Sol session cookie is still accepted by the node (an admin read). */
    private static boolean sessionLive(String cookie) {
        if (cookie == null || !cookie.contains("sol_session=")) return false;
        try {
            HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT + "/admin/v1/status").openConnection();
            c.setRequestProperty("Host", "localhost:" + NodeService.PORT);
            c.setRequestProperty("Origin", "http://localhost:" + NodeService.PORT);
            c.setRequestProperty("Cookie", cookie);
            try { return c.getResponseCode() == 200; } finally { c.disconnect(); }
        } catch (java.io.IOException e) {
            return false;
        }
    }

    /**
     * Signs the WebView in on an app origin (Files, Notes, a contact's app) and opens it there.
     *
     * The daemon's own flow is a redirect chain: the app origin sends an unsigned page load to
     * the root's /_sol/open, which mints a one-time code and redirects to /_sol/login?code= on
     * the app origin, which sets its sol_app cookie (SameSite=Strict). Chromium counts
     * files.localhost and localhost as different sites ("localhost" has no registrable domain),
     * so the WebView neither keeps nor sends a Strict cookie that arrived through that cross-site
     * chain, and the page redirects forever. The same two requests are made here instead, with
     * the root session's cookie, and the resulting sol_app cookie is stored for exactly that
     * origin, HttpOnly (Lax: the daemon's Origin / Sec-Fetch checks are the CSRF guard). The
     * code, the session and the cookie are never logged.
     */
    private void openApp(Uri open) {
        String rootCookie = CookieManager.getInstance().getCookie(HOME);
        new Thread(() -> {
            String target = null;
            try {
                String[] first = get(HOME.substring(0, HOME.length() - 1) + open.getEncodedPath()
                        + (open.getEncodedQuery() != null ? "?" + open.getEncodedQuery() : ""), rootCookie);
                Uri login = first[0] == null ? null : Uri.parse(first[0]);
                if (login != null && isLocal(login) && "/_sol/login".equals(login.getPath())) {
                    String origin = "http://" + login.getHost() + ":" + NodeService.PORT;
                    String[] second = get(login.toString(), null);
                    if (second[1] != null) {
                        CookieManager cm = CookieManager.getInstance();
                        cm.setCookie(origin, second[1] + "; Path=/; Max-Age=2592000; HttpOnly");
                        cm.flush();
                        String next = second[0] != null && second[0].startsWith("/") ? second[0] : "/";
                        target = origin + next;
                    }
                }
            } catch (java.io.IOException ignored) { }
            String go = target;
            act.runOnUiThread(() -> {
                if (web == null) return;
                if (go != null) { openFailures = 0; web.loadUrl(go); return; }
                // The root session is gone: sign in again and retry the app, but not forever.
                String to = open.getQueryParameter("to"), next = open.getQueryParameter("next");
                String retry = ++openFailures <= 2 && to != null && to.matches("[a-z0-9][a-z0-9.-]*")
                        ? "http://" + to + "." + ORIGIN_SUFFIX + ":" + NodeService.PORT
                                + (next != null && next.startsWith("/") && !next.startsWith("//") ? next : "/")
                        : HOME;
                show(retry);
            });
        }, "sol-open-app").start();
    }

    /** The web address of an installed app of this node. */
    static String appUrl(String id) { return "http://" + id + "." + ORIGIN_SUFFIX + ":" + NodeService.PORT + "/"; }

    /**
     * One GET to the node on loopback without following redirects; returns {Location,
     * "sol_app=..." from Set-Cookie}. The URL's host goes in the Host header (the node routes
     * on it), the connection itself is to 127.0.0.1.
     */
    private static String[] get(String url, String cookie) throws java.io.IOException {
        Uri u = Uri.parse(url);
        HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT
                + u.getEncodedPath() + (u.getEncodedQuery() != null ? "?" + u.getEncodedQuery() : "")).openConnection();
        c.setInstanceFollowRedirects(false);
        c.setRequestProperty("Host", u.getHost() + ":" + NodeService.PORT);
        c.setRequestProperty("Sec-Fetch-Mode", "navigate");
        if (cookie != null) c.setRequestProperty("Cookie", cookie);
        try {
            c.getResponseCode();
            String app = null;
            java.util.List<String> set = c.getHeaderFields().get("Set-Cookie");
            if (set != null) for (String h : set) {
                String kv = h.split(";", 2)[0].trim();
                if (kv.startsWith("sol_app=")) app = kv;
            }
            return new String[]{c.getHeaderField("Location"), app};
        } finally {
            c.disconnect();
        }
    }

    /** Back inside the page's history; false when the page is at its start. */
    boolean back() {
        if (web != null && web.canGoBack()) { web.goBack(); return true; }
        return false;
    }

    /** Hands a file picker's result to the page that asked. */
    /** The activity's onRequestPermissionsResult: finishes turning calendar sync on, or tells the page it stayed off. */
    void permissionsResult(int req, int[] grants) {
        boolean ok = grants.length > 0;
        for (int g : grants) ok &= g == android.content.pm.PackageManager.PERMISSION_GRANTED;
        if (req == REQ_WIFI_DIRECT) {
            if (ok) setWifiDirect(true);
            else wifiDirectChanged(false, "the Nearby devices permission was refused");
            return;
        }
        if (req != REQ_CALENDAR) return;
        if (ok) setCalendarSync(true);
        else calendarSyncChanged(false);
    }

    /**
     * Turns calendar sync on (asking for the calendar permissions first when needed) or off, then
     * tells the page with a "sol-calendar-sync" event (detail.on) on window.
     */
    void setCalendarSync(boolean on) {
        if (on && !CalendarSync.permitted(act)) {
            act.requestPermissions(CalendarSync.PERMISSIONS, REQ_CALENDAR);
            return;
        }
        android.content.Context app = act.getApplicationContext();
        new Thread(() -> {
            boolean now;
            if (on) now = CalendarSync.enable(app);
            else { CalendarSync.disable(app); now = false; }
            act.runOnUiThread(() -> calendarSyncChanged(now));
        }, "sol-calendar-switch").start();
    }

    /**
     * The Wi-Fi Direct switch: on asks for the permission it needs first (Nearby devices on
     * Android 13+, location below), then runs `sol do sol.nearby --wifi_direct on|off`; the
     * outcome arrives as a "sol-wifi-direct" event on window (detail.on, detail.error).
     */
    void setWifiDirect(boolean on) {
        if (on && !WifiDirect.permitted(act)) {
            act.requestPermissions(new String[]{WifiDirect.PERMISSION}, REQ_WIFI_DIRECT);
            return;
        }
        if (on) WifiDirect.permissionGiven(act);
        android.content.Context app = act.getApplicationContext();
        new Thread(() -> {
            String[] r = NodeService.cli2(app, "do", "sol.nearby", "--wifi_direct", on ? "on" : "off", "--json");
            boolean ok = r[0].equals("0");
            act.runOnUiThread(() -> wifiDirectChanged(ok ? on : WifiDirect.wanted(app), ok ? "" : r[1]));
        }, "sol-wifi-direct-switch").start();
    }

    private void wifiDirectChanged(boolean on, String error) {
        if (web == null) return;
        String u = web.getUrl();
        if (u == null || !isLocal(Uri.parse(u))) return;
        web.evaluateJavascript("window.dispatchEvent(new CustomEvent('sol-wifi-direct',{detail:{on:" + on
                + ",error:" + org.json.JSONObject.quote(error == null ? "" : error) + "}}))", null);
    }

    private void calendarSyncChanged(boolean on) {
        if (web == null) return;
        String u = web.getUrl();
        if (u == null || !isLocal(Uri.parse(u))) return;
        web.evaluateJavascript("window.dispatchEvent(new CustomEvent('sol-calendar-sync',{detail:{on:" + on + "}}))", null);
    }

    void activityResult(int req, int res, Intent data) {
        if (req == REQ_FILE && chooser != null) {
            chooser.onReceiveValue(chosenFiles(res, data));
            chooser = null;
        }
    }

    /**
     * The files a picker returned. Several files come back as ClipData, which the platform's
     * FileChooserParams.parseResult ignores (it reads only getData()), so an <input multiple>
     * received one file or none.
     */
    static Uri[] chosenFiles(int res, Intent data) {
        if (res != Activity.RESULT_OK || data == null) return null;
        android.content.ClipData clip = data.getClipData();
        if (clip != null && clip.getItemCount() > 0) {
            java.util.ArrayList<Uri> out = new java.util.ArrayList<>();
            for (int i = 0; i < clip.getItemCount(); i++) {
                Uri u = clip.getItemAt(i).getUri();
                if (u != null) out.add(u);
            }
            if (!out.isEmpty()) return out.toArray(new Uri[0]);
        }
        Uri one = data.getData();
        return one != null ? new Uri[] { one } : null;
    }

    static String versionName(android.content.Context c) {
        try {
            String v = c.getPackageManager().getPackageInfo(c.getPackageName(), 0).versionName;
            return v == null ? "0" : v;
        } catch (android.content.pm.PackageManager.NameNotFoundException e) {
            return "0";
        }
    }

    // ---- the page's bridge ----

    /**
     * window.SolAndroid: what a Sol page may ask of the shell. Every method answers only while the
     * top-level page is a local Sol origin (the launcher or an app on *.localhost:7777); the
     * WebView never shows other sites itself, but an embedded frame could call it.
     */
    private final class Bridge {
        private boolean local() {
            String u = pageUrl;
            return u != null && isLocal(Uri.parse(u));
        }

        /** True inside the Android app: the page can hide what makes no sense here (Sign out). */
        @android.webkit.JavascriptInterface
        public boolean isApp() { return local(); }

        /**
         * Opens a file of this node in the system viewer (a PDF reader, a video player): fetched
         * once into the app cache and handed over as a read-only content:// URI. url may be
         * relative to the page; it must resolve to a local Sol origin. mime and name may be empty.
         */
        @android.webkit.JavascriptInterface
        public boolean open(String url, String mime, String name) {
            String page = pageUrl;
            if (!local() || url == null) return false;
            Uri u;
            try { u = Uri.parse(new java.net.URL(new URL(page), url).toString()); }
            catch (java.net.MalformedURLException e) { return false; }
            if (!isLocal(u)) return false;
            act.runOnUiThread(() -> openNative(u.toString(), mime, name));
            return true;
        }

        /**
         * Settings, "Remove Sol from this phone": after a native confirmation, erases the node's
         * data and SolDrive and returns to the welcome screen (Start new, Add to my Sol, Restore).
         */
        @android.webkit.JavascriptInterface
        public void reset() {
            Runnable r = onReset;
            if (local() && r != null) act.runOnUiThread(r);
        }

        /**
         * Puts an app of this node on the home screen: Android asks the person to confirm, then
         * the icon (named after the app, with its icon) opens the app full screen in its own task
         * (AppActivity). appId is the app's id (files), name its display name. False when refused
         * (not a local page, a bad id, or a launcher without pinned shortcuts).
         */
        /**
         * The "Show in phone's calendar apps" switch: true turns calendar sync on (Android asks for
         * the calendar permissions first, once), false turns it off and removes Sol's calendars
         * from the phone. Answers at once whether the request was taken; the outcome arrives as a
         * "sol-calendar-sync" event on window with detail.on.
         */
        @android.webkit.JavascriptInterface
        public boolean calendarSync(boolean on) {
            if (!local()) return false;
            act.runOnUiThread(() -> setCalendarSync(on));
            return true;
        }

        /**
         * The Wi-Fi Direct switch (nearby without any network, phone to phone): true asks for
         * the Nearby devices permission first, then turns it on; false turns it off. Answers at
         * once whether the request was taken; the outcome is a "sol-wifi-direct" event on window
         * (detail.on, detail.error).
         */
        @android.webkit.JavascriptInterface
        public boolean wifiDirect(boolean on) {
            if (!local()) return false;
            act.runOnUiThread(() -> setWifiDirect(on));
            return true;
        }

        /** Whether Wi-Fi Direct is switched on (config.json; state: `sol do sol.nearby`). */
        @android.webkit.JavascriptInterface
        public boolean wifiDirectEnabled() { return local() && WifiDirect.wanted(act); }

        /** Whether the Nearby devices permission Wi-Fi Direct needs is granted. */
        @android.webkit.JavascriptInterface
        public boolean wifiDirectPermitted() { return local() && WifiDirect.permitted(act); }

        /** Whether Sol's calendars are in the phone's calendar apps now. */
        @android.webkit.JavascriptInterface
        public boolean calendarSyncEnabled() { return local() && CalendarSync.wanted(act); }

        /**
         * Opens an Android app connected to Sol (the Apps grid's tile for an external app). The
         * page names it by its Sol app id only; the package comes from the node's own app list
         * (`sol apps --json`), never from the page, so a page can launch only apps the person
         * connected. Answers at once whether the request was taken; failures show a toast.
         */
        @android.webkit.JavascriptInterface
        public boolean openApp(String appId) {
            if (!local() || !Shortcuts.validId(appId)) return false;
            new Thread(() -> launchExternal(appId), "sol-open-app").start();
            return true;
        }

        @android.webkit.JavascriptInterface
        public boolean addToHomeScreen(String appId, String name) {
            if (!local() || appId == null || !Shortcuts.validId(appId)) return false;
            return Shortcuts.pin(act, appId, name == null || name.trim().isEmpty() ? appId : name.trim());
        }
    }

    // ---- connected apps ----

    /** Launches external app appId: its package from the node's app list, then its launcher activity. */
    private void launchExternal(String appId) {
        String pkg = null;
        String[] r = NodeService.cli(act, "apps", "--json");
        if ("0".equals(r[0])) {
            try {
                org.json.JSONArray apps = new org.json.JSONArray(r[1]);
                for (int i = 0; i < apps.length(); i++) {
                    org.json.JSONObject a = apps.getJSONObject(i);
                    if (appId.equals(a.optString("id")) && a.optBoolean("external") && !a.optString("package").isEmpty()) {
                        pkg = a.optString("package");
                    }
                }
            } catch (org.json.JSONException ignored) { }
        }
        final String p = pkg;
        act.runOnUiThread(() -> {
            Intent in = p == null ? null : act.getPackageManager().getLaunchIntentForPackage(p);
            if (in == null) {
                Toast.makeText(act, p == null ? "That app is not connected to Sol" : "That app is not installed on this phone", Toast.LENGTH_LONG).show();
                return;
            }
            try { act.startActivity(in.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)); }
            catch (android.content.ActivityNotFoundException e) {
                Toast.makeText(act, "That app is not installed on this phone", Toast.LENGTH_LONG).show();
            }
        });
    }

    // ---- downloads ----

    /** Fetches a file from the node and saves it into Downloads. Done here, not by DownloadManager,
     *  because the node only answers on loopback and DownloadManager cannot resolve *.localhost. */
    private void download(String url, String disposition, String mime) {
        Uri u = Uri.parse(url);
        if (!isLocal(u)) return;
        String cookie = CookieManager.getInstance().getCookie(url);
        String name = fileName(url, disposition, mime);
        new Thread(() -> {
            try {
                HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT
                        + u.getEncodedPath() + (u.getEncodedQuery() != null ? "?" + u.getEncodedQuery() : "")).openConnection();
                c.setRequestProperty("Host", u.getHost() + ":" + NodeService.PORT);
                if (cookie != null) c.setRequestProperty("Cookie", cookie);
                try (InputStream in = c.getInputStream(); OutputStream out = openDownload(name, mime)) {
                    in.transferTo(out);
                }
                act.runOnUiThread(() -> Toast.makeText(act, "Saved " + name + " to Downloads", Toast.LENGTH_SHORT).show());
            } catch (Exception e) {
                act.runOnUiThread(() -> Toast.makeText(act, "Download failed", Toast.LENGTH_SHORT).show());
            }
        }, "sol-download").start();
    }

    /**
     * Whether the WebView's download is one the user asked to save: Files marks those with
     * download=1 (and the node then sends Content-Disposition: attachment). Anything else reached
     * the download listener only because the WebView cannot render it inline (a PDF preview).
     */
    static boolean explicitDownload(String url, String disposition) {
        Uri u = Uri.parse(url);
        if ("1".equals(u.getQueryParameter("download"))) return true;
        return disposition != null && disposition.trim().toLowerCase(java.util.Locale.ROOT).startsWith("attachment");
    }

    /** The last native open per URL, so a preview that fires several times opens once. */
    private final java.util.Map<String, Long> recentOpens = new java.util.HashMap<>();

    /**
     * Opens a file of the node in the system viewer instead of saving it to Downloads: fetched
     * into cacheDir/open/ (reused when the URL, length and ETag match) and handed over by
     * OpenProvider as a read-only content:// URI. The same URL within 4 seconds opens once.
     */
    private void openNative(String url, String mime, String name) {
        Uri u = Uri.parse(url);
        if (!isLocal(u)) return;
        long now = System.currentTimeMillis();
        Long last = recentOpens.get(url);
        if (last != null && now - last < 4000) return;
        recentOpens.put(url, now);
        String cookie = CookieManager.getInstance().getCookie(url);
        new Thread(() -> {
            try {
                HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:" + NodeService.PORT
                        + u.getEncodedPath() + (u.getEncodedQuery() != null ? "?" + u.getEncodedQuery() : "")).openConnection();
                c.setRequestProperty("Host", u.getHost() + ":" + NodeService.PORT);
                if (cookie != null) c.setRequestProperty("Cookie", cookie);
                File f;
                String type;
                try {
                    if (c.getResponseCode() != 200) throw new java.io.IOException("status " + c.getResponseCode());
                    type = mime != null && !mime.isEmpty() && !mime.startsWith("application/octet-stream") ? mime
                            : c.getContentType() != null ? c.getContentType().split(";")[0].trim() : "application/octet-stream";
                    String n = name != null && !name.isEmpty() ? sanitize(name)
                            : fileName(url, c.getHeaderField("Content-Disposition"), type);
                    String key = OpenProvider.key(url + "|" + c.getContentLengthLong() + "|" + c.getHeaderField("ETag")
                            + "|" + c.getHeaderField("Last-Modified"));
                    f = OpenProvider.file(act, key, n);
                    if (!f.exists() || (c.getContentLengthLong() >= 0 && f.length() != c.getContentLengthLong())) {
                        OpenProvider.prune(act);
                        f.getParentFile().mkdirs();
                        File tmp = new File(f.getPath() + ".part");
                        try (InputStream in = c.getInputStream(); OutputStream out = new FileOutputStream(tmp)) {
                            in.transferTo(out);
                        }
                        if (!tmp.renameTo(f)) throw new java.io.IOException("cannot store");
                    }
                    f.setLastModified(System.currentTimeMillis()); // kept by prune while in use
                    f.getParentFile().setLastModified(System.currentTimeMillis());
                } finally {
                    c.disconnect();
                }
                Uri content = OpenProvider.uri(f);
                act.runOnUiThread(() -> {
                    Intent v = new Intent(Intent.ACTION_VIEW).setDataAndType(content, type)
                            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    try { act.startActivity(v); }
                    catch (android.content.ActivityNotFoundException e) {
                        Toast.makeText(act, "No app on this phone can open this file", Toast.LENGTH_LONG).show();
                    }
                });
            } catch (Exception e) {
                act.runOnUiThread(() -> Toast.makeText(act, "Could not open the file", Toast.LENGTH_SHORT).show());
            }
        }, "sol-open").start();
    }

    /**
     * The download's file name. Files sends only the RFC 6266 form
     * {@code filename*=UTF-8''<percent-encoded>} (it is the one that carries any name), which
     * URLUtil.guessFileName does not read: it would save every download as "raw.txt".
     */
    static String fileName(String url, String disposition, String mime) {
        if (disposition != null) {
            java.util.regex.Matcher m = java.util.regex.Pattern
                    .compile("filename\\*\\s*=\\s*UTF-8''([^;]+)", java.util.regex.Pattern.CASE_INSENSITIVE).matcher(disposition);
            if (m.find()) {
                try {
                    String n = sanitize(java.net.URLDecoder.decode(m.group(1).trim().replace("+", "%2B"), "UTF-8"));
                    if (!n.equals("shared")) return n;
                } catch (java.io.UnsupportedEncodingException | IllegalArgumentException ignored) { }
            }
        }
        return URLUtil.guessFileName(url, disposition, mime);
    }

    private OutputStream openDownload(String name, String mime) throws java.io.IOException {
        if (Build.VERSION.SDK_INT >= 29) {
            ContentValues v = new ContentValues();
            v.put(MediaStore.Downloads.DISPLAY_NAME, name);
            v.put(MediaStore.Downloads.MIME_TYPE, mime);
            v.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS);
            Uri row = act.getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
            return act.getContentResolver().openOutputStream(row);
        }
        File d = act.getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS);
        d.mkdirs();
        return new FileOutputStream(new File(d, name));
    }

    /** A file name with no path parts, so a hostile sender cannot write outside its folder. */
    static String sanitize(String n) {
        n = n.replace('/', '_').replace('\\', '_');
        return n.isEmpty() || n.equals("..") || n.equals(".") ? "shared" : n;
    }
}
