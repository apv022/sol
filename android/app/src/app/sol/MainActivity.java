package app.sol;

import android.Manifest;
import android.app.Activity;
import android.content.BroadcastReceiver;
import android.content.ContentValues;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.ApplicationInfo;
import android.database.Cursor;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Environment;
import android.provider.MediaStore;
import android.provider.OpenableColumns;
import android.text.InputType;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.view.inputmethod.EditorInfo;
import android.webkit.CookieManager;
import android.webkit.URLUtil;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;

/**
 * The one screen: a first-run name form, then a WebView showing Sol's own UIs.
 *
 * The launcher is http://localhost:7777/ and each app is http://&lt;app&gt;.localhost:7777/ in the same
 * WebView (Chromium resolves *.localhost to loopback itself). The page draws under the system bars
 * and the activity pads it by the bar insets and the keyboard (WebView does not supply them to the
 * kit as env()). Shared files are copied into SolDrive/Inbox-from-share and Files is opened.
 */
public class MainActivity extends Activity {
    private static final String ORIGIN_SUFFIX = "localhost";
    private static final String HOME = "http://localhost:" + NodeService.PORT + "/";
    private static final int REQ_FILE = 1, REQ_NOTIF = 2;

    private FrameLayout root;
    private WebView web;
    private View setup;
    private ValueCallback<Uri[]> chooser;
    private String pendingUrl;
    private boolean loaded;
    private final BroadcastReceiver readyReceiver = new BroadcastReceiver() {
        @Override public void onReceive(Context c, Intent i) { if (!joinPending() && setup == null) showWeb(); }
    };

    @Override
    protected void onCreate(android.os.Bundle b) {
        super.onCreate(b);
        root = new FrameLayout(this);
        root.setBackgroundResource(R.color.win_bg);
        setContentView(root);
        edgeToEdge();
        if (Build.VERSION.SDK_INT >= 33) requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIF);
        registerReceiver(readyReceiver, new IntentFilter(NodeService.ACTION_READY), Context.RECEIVER_NOT_EXPORTED);
        start(getIntent());
    }

    @Override
    protected void onNewIntent(Intent in) {
        super.onNewIntent(in);
        setIntent(in);
        Uri link = solLink(in);
        if (link != null) {
            if (!NodeService.initialised(this) || joinPending()) start(in);
            else { buildWeb(); handleLink(link); }
            return;
        }
        handleShare(in);
    }

    @Override
    protected void onDestroy() {
        unregisterReceiver(readyReceiver);
        if (web != null) web.destroy();
        super.onDestroy();
    }

    private void start(Intent in) {
        boolean debuggable = (getApplicationInfo().flags & ApplicationInfo.FLAG_DEBUGGABLE) != 0;
        String name = in.getStringExtra(NodeService.EXTRA_NAME);
        Uri link = solLink(in);
        if (!NodeService.initialised(this)) {
            if (name != null && debuggable) { // a debug build accepts `--es name X` so tests can skip the form
                startNode(name, null, null);
                buildWeb();
                handleShare(in);
                return;
            }
            if (link != null && "join".equals(link.getHost())) {
                showJoin(link.toString(), true); // a fresh install opened from a join link: straight in
            } else {
                if (link != null) pendingLink = link.toString(); // a pair link waits for setup
                showWelcome();
            }
            return;
        }
        if (joinPending()) {
            showJoin(link != null && "join".equals(link.getHost()) ? link.toString() : null,
                    link != null && "join".equals(link.getHost()));
            return;
        }
        startNode(null, null, null);
        buildWeb();
        handleShare(in);
        if (NodeService.isReady()) showWeb();
        if (link != null) handleLink(link);
    }

    private void startNode(String name, String mode, String device) {
        Intent s = new Intent(this, NodeService.class);
        if (name != null || mode != null) {
            s.putExtra(NodeService.EXTRA_NAME, name == null ? "" : name);
            s.putExtra(NodeService.EXTRA_MODE, mode == null ? "" : mode);
            s.putExtra(NodeService.EXTRA_DEVICE, device == null ? "" : device);
        }
        startForegroundService(s);
    }

    // ---- links (sol://join and sol://pair) ----

    private static Uri solLink(Intent in) {
        Uri u = Intent.ACTION_VIEW.equals(in.getAction()) ? in.getData() : null;
        return u != null && "sol".equals(u.getScheme()) && ("join".equals(u.getHost()) || "pair".equals(u.getHost())) ? u : null;
    }

    /** True from `init --join` until the join link has been redeemed. */
    private boolean joinPending() { return getSharedPreferences("sol", MODE_PRIVATE).getBoolean("joinPending", false); }

    private void setJoinPending(boolean v) { getSharedPreferences("sol", MODE_PRIVATE).edit().putBoolean("joinPending", v).apply(); }

    /** A link that arrived while the app runs (or after setup). */
    private void handleLink(Uri link) {
        setIntent(new Intent());
        if ("join".equals(link.getHost())) {
            if (joinPending()) showJoin(link.toString(), true);
            else toast("This device already belongs to a Sol. Join links are for a new device.");
            return;
        }
        if (joinPending()) { toast("Finish adding this device first, then open the pair link again."); return; }
        String who = link.getQueryParameter("n");
        String shown = who == null || who.trim().isEmpty() ? "this person" : who.trim();
        String text = link.toString();
        new android.app.AlertDialog.Builder(this)
                .setTitle("Pair with " + shown + "?")
                .setMessage("You will be able to share with each other.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Pair", (d, w) -> runPair(text, false))
                .show();
    }

    /**
     * Redeems a link with `libsol.so pair`, waiting for the node to serve first, and shows the CLI's
     * own text as the result. A join then opens the launcher.
     */
    private void runPair(String link, boolean join) {
        TextView status = join ? joinStatus : null;
        if (status != null) status.setText("Joining…");
        else toast("Pairing…");
        if (join && joinSpinner != null) joinSpinner.setVisibility(View.VISIBLE);
        new Thread(() -> {
            for (int i = 0; i < 300 && !NodeService.isReady(); i++) {
                try { Thread.sleep(100); } catch (InterruptedException e) { return; }
            }
            String[] r = NodeService.ready() ? NodeService.cli(this, "pair", link) : new String[]{"-1", "The node did not start."};
            boolean ok = r[0].equals("0");
            if (ok && !join) r[1] = pairedText(r[1]);
            runOnUiThread(() -> {
                if (join && joinSpinner != null) joinSpinner.setVisibility(View.GONE);
                if (ok && join) {
                    setJoinPending(false);
                    startService(new Intent(this, NodeService.class).setAction(NodeService.ACTION_JOINED));
                    joinedSummary(r[1], status);
                } else if (ok) {
                    notice("Paired", r[1]);
                } else if (status != null) {
                    status.setText(failure(r[1], "Joining failed."));
                    joinGo.setEnabled(true);
                } else {
                    notice("Could not pair", failure(r[1], "Pairing failed."));
                }
            });
        }, "sol-pair").start();
    }

    /**
     * "Paired with <person ID>" from the CLI, said with the person's name: the link carries no
     * name, but the handshake has just exchanged it, so `people --json` knows it. Runs off the UI
     * thread. Falls back to a sentence without the ID, which means nothing to a person.
     */
    private String pairedText(String cli) {
        java.util.regex.Matcher m = java.util.regex.Pattern.compile("[a-z2-7]{52}").matcher(cli);
        if (!m.find()) return cli;
        String id = m.group();
        String[] r = NodeService.cli(this, "people", "--json");
        if (r[0].equals("0")) {
            try {
                org.json.JSONArray a = new org.json.JSONArray(r[1]);
                for (int i = 0; i < a.length(); i++) {
                    org.json.JSONObject p = a.getJSONObject(i);
                    String name = p.optString("name", "").trim();
                    if (id.equals(p.optString("id")) && !name.isEmpty()) return "You and " + name + " can now share with each other.";
                }
            } catch (org.json.JSONException ignored) { }
        }
        return "You can now share with each other.";
    }

    /** The CLI's error text without its "sol:" prefix, or the fallback when there is none. */
    private static String failure(String cli, String fallback) {
        String t = cli == null ? "" : cli.trim();
        if (t.startsWith("sol: ")) t = t.substring(5);
        if (t.startsWith("bad_request: ")) t = t.substring(13);
        String low = t.toLowerCase(java.util.Locale.ROOT);
        // The CLI's text is for a terminal; say what the person can do about the common cases.
        if (low.contains("not a valid")) return "That is not a Sol link. Copy the whole sol:// link and try again.";
        // A used, expired or unknown secret is refused in the handshake ("rejected the connection").
        if (low.contains("invalid or expired") || low.contains("expired") || low.contains("rejected the connection"))
            return "This link has expired or was already used. Ask for a new one.";
        if (low.contains("link version") || low.contains("unsupported version")) return "This link is from a different version of Sol. Update Sol on both devices.";
        if (low.contains("unreachable") || low.contains("timeout") || low.contains("deadline") || low.contains("refused")
                || low.contains("no route") || low.contains("unavailable"))
            return "Could not reach the other device. Check that Sol is open on it and that both are online, then try again.";
        return t.isEmpty() ? fallback : t;
    }

    /** A message that stays until dismissed (a toast is too short for an error to be read). */
    private void notice(String title, String text) {
        new android.app.AlertDialog.Builder(this).setTitle(title).setMessage(text)
                .setPositiveButton("OK", null).show();
    }

    private void joinedSummary(String cliText, TextView status) {
        // The CLI says "Joined. This device now belongs to <person>"; show it as "<person>'s <device>".
        String person = cliText.contains("belongs to ") ? cliText.substring(cliText.indexOf("belongs to ") + 11).trim() : null;
        String device = deviceField != null ? deviceField.getText().toString().trim() : Build.MODEL;
        // Right after joining, the person's name may not have synced yet and the CLI names them by
        // ID; a 52-character key means nothing to a person, so leave it out.
        if (person != null && person.matches("[a-z2-7]{40,}")) person = null;
        status.setText(person == null ? "Joined. This phone is now one of your devices." : "Joined as " + person + "'s " + device);
        root.postDelayed(() -> {
            if (setup != null) root.removeView(setup);
            setup = null;
            buildWeb();
            showWeb();
        }, 1800);
    }

    private void toast(String t) { Toast.makeText(this, t, Toast.LENGTH_LONG).show(); }

    // ---- first run ----

    private String pendingLink;
    private TextView joinStatus;
    private View joinSpinner;
    private Button joinGo;
    private EditText deviceField;

    private void show(View v) {
        if (setup != null) root.removeView(setup);
        setup = v;
        root.addView(v, new FrameLayout.LayoutParams(-1, -1));
    }

    private LinearLayout column(String title, String sub) {
        LinearLayout col = new LinearLayout(this);
        col.setOrientation(LinearLayout.VERTICAL);
        col.setGravity(Gravity.CENTER_VERTICAL);
        int p = dp(28);
        col.setPadding(p, p, p, p);
        TextView t = new TextView(this);
        t.setText(title);
        t.setTextSize(30);
        t.setTextColor(textColor());
        TextView s = new TextView(this);
        s.setText(sub);
        s.setTextSize(16);
        s.setPadding(0, dp(8), 0, dp(24));
        s.setTextColor(textColor() & 0x00ffffff | 0xb0000000);
        col.addView(t);
        col.addView(s);
        return col;
    }

    /** The two ways in: a new Sol, or this phone as another device of an existing person. */
    private void showWelcome() {
        LinearLayout col = column("Welcome to Sol", "Start on your own, or add this phone to the Sol you already have.");
        Button fresh = new Button(this);
        fresh.setText("Start new");
        fresh.setOnClickListener(v -> showNewName());
        Button join = new Button(this);
        join.setText("Add to my Sol");
        join.setOnClickListener(v -> showJoin(null, false));
        col.addView(fresh);
        col.addView(join);
        show(col);
    }

    private void showNewName() {
        LinearLayout col = column("Welcome to Sol", "What should your devices and the people you share with call you?");
        EditText name = new EditText(this);
        name.setHint("Your name");
        name.setSingleLine();
        name.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_WORDS);
        name.setImeOptions(EditorInfo.IME_ACTION_DONE);
        name.setTextColor(textColor());
        Button go = new Button(this);
        go.setText("Continue");
        Button back = new Button(this);
        back.setText("Back");
        back.setOnClickListener(v -> showWelcome());
        Runnable submit = () -> {
            String n = name.getText().toString().trim();
            if (n.isEmpty()) { name.setError("Enter a name"); return; }
            root.removeView(setup);
            setup = null;
            startNode(n, null, null);
            buildWeb();
            handleShare(getIntent());
            if (pendingLink != null) { // a pair link that opened the app before setup
                Uri l = Uri.parse(pendingLink);
                pendingLink = null;
                handleLink(l);
            }
        };
        go.setOnClickListener(v -> submit.run());
        name.setOnEditorActionListener((v, a, e) -> { submit.run(); return true; });
        col.addView(name);
        col.addView(go);
        col.addView(back);
        show(col);
        name.requestFocus();
    }

    /**
     * "Add to my Sol": a device name (default the phone model) and the join link, pasted or
     * delivered by a sol://join tap or QR scan. With a link in hand it starts at once.
     */
    private void showJoin(String link, boolean auto) {
        LinearLayout col = column("Add to my Sol", "On your other device open Sol, choose Add my device, and scan the QR code with the camera app, or paste the link here.");
        deviceField = new EditText(this);
        deviceField.setSingleLine();
        deviceField.setText(Build.MODEL);
        deviceField.setHint("Device name");
        deviceField.setTextColor(textColor());
        EditText linkField = new EditText(this);
        linkField.setHint("Paste link (sol://join…)");
        linkField.setSingleLine();
        // A link, not words: no autocorrect or suggestions mangling a pasted or typed link.
        linkField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        linkField.setImeOptions(EditorInfo.IME_ACTION_GO);
        linkField.setTextColor(textColor());
        if (link != null) linkField.setText(link);
        joinGo = new Button(this);
        joinGo.setText("Join");
        joinStatus = new TextView(this);
        joinStatus.setTextSize(16);
        joinStatus.setPadding(0, dp(16), 0, 0);
        joinStatus.setTextColor(textColor());
        android.widget.ProgressBar spin = new android.widget.ProgressBar(this);
        spin.setVisibility(View.GONE);
        joinSpinner = spin;
        Button back = new Button(this);
        back.setText(NodeService.initialised(this) ? "Start new instead" : "Back");
        back.setOnClickListener(v -> {
            if (!NodeService.initialised(this)) showWelcome();
            else abandonJoin();
        });
        Runnable submit = () -> {
            String l = linkField.getText().toString().trim();
            if (!l.startsWith("sol://join")) { linkField.setError("Paste a sol://join link"); return; }
            String dev = deviceField.getText().toString().trim();
            joinGo.setEnabled(false);
            back.setText("Start new instead");
            if (!NodeService.initialised(this) && !joinPending()) {
                setJoinPending(true);
                startNode("", "join", dev.isEmpty() ? Build.MODEL : dev);
            } else {
                startNode(null, null, null);
            }
            runPair(l, true);
        };
        joinGo.setOnClickListener(v -> submit.run());
        linkField.setOnEditorActionListener((v, a, e) -> { if (joinGo.isEnabled()) submit.run(); return true; });
        col.addView(deviceField);
        col.addView(linkField);
        col.addView(joinGo);
        col.addView(spin);
        col.addView(joinStatus);
        col.addView(back);
        show(col);
        if (auto) submit.run();
    }

    /**
     * "Start new instead" on a join that has not completed: stops the node, deletes the half-made
     * identity (nothing has been shared yet) and returns to the welcome screen, so "Start new"
     * then makes a fresh person instead of staying stuck as a device waiting for a link.
     */
    private void abandonJoin() {
        new android.app.AlertDialog.Builder(this)
                .setTitle("Start a new Sol instead?")
                .setMessage("This device will not be added to your other Sol.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Start new", (d, w) -> new Thread(() -> {
                    stopService(new Intent(this, NodeService.class));
                    for (int i = 0; i < 100 && NodeService.isAlive(); i++) {
                        try { Thread.sleep(100); } catch (InterruptedException e) { return; }
                    }
                    deleteTree(NodeService.dataDir(this));
                    getSharedPreferences("sol", MODE_PRIVATE).edit().clear().apply();
                    runOnUiThread(this::showWelcome);
                }, "sol-abandon").start())
                .show();
    }

    private static void deleteTree(File f) {
        File[] kids = f.listFiles();
        if (kids != null) for (File k : kids) deleteTree(k);
        f.delete();
    }

    private int textColor() {
        boolean night = (getResources().getConfiguration().uiMode & android.content.res.Configuration.UI_MODE_NIGHT_MASK)
                == android.content.res.Configuration.UI_MODE_NIGHT_YES;
        return night ? Color.WHITE : Color.BLACK;
    }

    private int dp(int v) { return Math.round(v * getResources().getDisplayMetrics().density); }

    // ---- window ----

    /**
     * Draws under transparent bars and pads the content by the bar insets (and the keyboard).
     * Android's WebView does not fill the kit's env(safe-area-inset-*) with the system bars, so
     * the padding is applied natively; the window background (the theme's page colour) shows
     * in the padded strips.
     */
    private void edgeToEdge() {
        root.setSystemUiVisibility(View.SYSTEM_UI_FLAG_LAYOUT_STABLE | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION);
        root.setOnApplyWindowInsetsListener((v, ins) -> {
            int top, bottom;
            if (Build.VERSION.SDK_INT >= 30) {
                top = ins.getInsets(WindowInsets.Type.statusBars()).top;
                bottom = Math.max(ins.getInsets(WindowInsets.Type.navigationBars()).bottom,
                        ins.getInsets(WindowInsets.Type.ime()).bottom);
            } else {
                top = ins.getSystemWindowInsetTop();
                bottom = ins.getSystemWindowInsetBottom();
            }
            v.setPadding(0, top, 0, bottom);
            return ins;
        });
        if (Build.VERSION.SDK_INT >= 30) getWindow().setDecorFitsSystemWindows(false);
    }

    // ---- the web view ----

    private void buildWeb() {
        if (web != null) return;
        web = new WebView(this);
        web.setBackgroundColor(Color.TRANSPARENT);
        web.setVisibility(View.INVISIBLE);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
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
                try { startActivity(new Intent(Intent.ACTION_VIEW, u)); } catch (RuntimeException ignored) { }
                return true;
            }

            @Override
            public void onPageFinished(WebView v, String url) {
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
                    startActivityForResult(p.createIntent(), REQ_FILE);
                } catch (RuntimeException e) {
                    chooser = null;
                    return false;
                }
                return true;
            }
        });
        web.setDownloadListener((url, ua, cd, mime, len) -> download(url, cd, mime));
        root.addView(web, 0, new FrameLayout.LayoutParams(-1, -1));
    }

    /** Whether the URL is this device's own UI: localhost or *.localhost on Sol's port. */
    private static boolean isLocal(Uri u) {
        String h = u.getHost();
        return "http".equals(u.getScheme()) && h != null && u.getPort() == NodeService.PORT
                && (h.equals(ORIGIN_SUFFIX) || h.endsWith("." + ORIGIN_SUFFIX));
    }

    /**
     * Shows the UI once the node is serving. The WebView is logged into the Sol app with a one-time
     * login code from `libsol.so open --print`, which works on every Android version. (The daemon's
     * same-user auto-login reads /proc/net/tcp, which Android 10+ hides from apps, so it cannot be
     * relied on.) The session cookie outlives the process, so a restart whose cookie the node
     * still accepts just loads the page; a fresh code is fetched only when it does not. App
     * origins are signed in from this session on first use (openApp).
     */
    private void showWeb() {
        if (web == null) return;
        final String target = pendingUrl != null ? pendingUrl : HOME;
        pendingUrl = null;
        web.setVisibility(View.VISIBLE);
        loaded = true;
        String cookie = CookieManager.getInstance().getCookie(HOME);
        new Thread(() -> {
            if (sessionLive(cookie)) { // signed in from an earlier start: no new session
                runOnUiThread(() -> { if (web != null) web.loadUrl(target); });
                return;
            }
            String[] r = NodeService.cli(this, "open", "--print");
            String last = r[1].contains("\n") ? r[1].substring(r[1].lastIndexOf('\n') + 1) : r[1];
            String login = r[0].equals("0") && last.startsWith(HOME + "login?code=") ? last : null;
            runOnUiThread(() -> {
                if (web == null) return;
                if (login != null) { afterLogin = target.equals(HOME) ? null : target; web.loadUrl(login); }
                else web.loadUrl(target); // no code: fall back to same-user auto-login
            });
        }, "sol-login").start();
    }

    private String afterLogin;

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
            runOnUiThread(() -> {
                if (web == null) return;
                if (go != null) web.loadUrl(go);
                else showWeb(); // the root session is gone: sign in again, then go home
            });
        }, "sol-open-app").start();
    }

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

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        if (req == REQ_FILE && chooser != null) {
            chooser.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(res, data));
            chooser = null;
        }
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
                runOnUiThread(() -> Toast.makeText(this, "Saved " + name + " to Downloads", Toast.LENGTH_SHORT).show());
            } catch (Exception e) {
                runOnUiThread(() -> Toast.makeText(this, "Download failed", Toast.LENGTH_SHORT).show());
            }
        }, "sol-download").start();
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
            Uri row = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
            return getContentResolver().openOutputStream(row);
        }
        File d = getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS);
        d.mkdirs();
        return new FileOutputStream(new File(d, name));
    }

    // ---- share target ----

    /** Copies files shared to Sol into SolDrive/Inbox-from-share (Files syncs it) and opens Files. */
    private void handleShare(Intent in) {
        String a = in.getAction();
        if (!Intent.ACTION_SEND.equals(a) && !Intent.ACTION_SEND_MULTIPLE.equals(a)) return;
        setIntent(new Intent()); // handle each share once
        ArrayList<Uri> uris = new ArrayList<>();
        if (Intent.ACTION_SEND.equals(a)) {
            Uri one = in.getParcelableExtra(Intent.EXTRA_STREAM);
            if (one != null) uris.add(one);
        } else {
            ArrayList<Uri> many = in.getParcelableArrayListExtra(Intent.EXTRA_STREAM);
            if (many != null) uris.addAll(many);
        }
        String text = in.getStringExtra(Intent.EXTRA_TEXT);
        pendingUrl = "http://files.localhost:" + NodeService.PORT + "/";
        new Thread(() -> {
            File dir = new File(NodeService.driveDir(this), "Inbox-from-share");
            dir.mkdirs();
            int n = 0;
            for (Uri u : uris) {
                try (InputStream is = getContentResolver().openInputStream(u);
                     OutputStream os = new FileOutputStream(unique(dir, displayName(u)))) {
                    is.transferTo(os);
                    n++;
                } catch (Exception ignored) { }
            }
            if (uris.isEmpty() && text != null) {
                try (OutputStream os = new FileOutputStream(unique(dir, "Shared text.txt"))) {
                    os.write(text.getBytes(java.nio.charset.StandardCharsets.UTF_8));
                    n++;
                } catch (Exception ignored) { }
            }
            int saved = n;
            runOnUiThread(() -> {
                Toast.makeText(this, saved == 0 ? "Nothing to save" : "Saved " + saved + " to Files", Toast.LENGTH_SHORT).show();
                if (NodeService.isReady() && web != null) showWeb();
            });
        }, "sol-share").start();
    }

    private String displayName(Uri u) {
        try (Cursor c = getContentResolver().query(u, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst() && !c.isNull(0)) return sanitize(c.getString(0));
        } catch (RuntimeException ignored) { }
        String last = u.getLastPathSegment();
        return sanitize(last == null ? "shared" : last);
    }

    /** A file name with no path parts, so a hostile sender cannot write outside Inbox-from-share. */
    private static String sanitize(String n) {
        n = n.replace('/', '_').replace('\\', '_');
        return n.isEmpty() || n.equals("..") || n.equals(".") ? "shared" : n;
    }

    /** dir/name, or dir/name (2).ext when that exists; never overwrites. */
    private static File unique(File dir, String name) {
        File f = new File(dir, name);
        int dot = name.lastIndexOf('.');
        String base = dot > 0 ? name.substring(0, dot) : name, ext = dot > 0 ? name.substring(dot) : "";
        for (int i = 2; f.exists(); i++) f = new File(dir, base + " (" + i + ")" + ext);
        return f;
    }
}
