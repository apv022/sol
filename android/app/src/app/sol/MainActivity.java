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
 * WebView (Chromium resolves *.localhost to loopback itself). The page draws under transparent
 * system bars and is told their size as --sol-inset-* CSS properties (WebView does not supply
 * them to the kit as env()); only the keyboard pads it natively. Shared files are copied into
 * SolDrive/Inbox-from-share and Files is opened. Pages can reach the shell through
 * window.SolAndroid (SolBridge).
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
        padSetup();
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

    /** The ways in: a new Sol, this phone as another device of an existing person, or a restore. */
    private void showWelcome() {
        LinearLayout col = column("Welcome to Sol", "Start on your own, add this phone to the Sol you already have, or restore from your backup words.");
        Button fresh = new Button(this);
        fresh.setText("Start new");
        fresh.setOnClickListener(v -> showNewName());
        Button join = new Button(this);
        join.setText("Add to my Sol");
        join.setOnClickListener(v -> showJoin(null, false));
        Button restore = new Button(this);
        restore.setText("Restore from backup words");
        restore.setOnClickListener(v -> showRestore());
        col.addView(fresh);
        col.addView(join);
        col.addView(restore);
        show(col);
    }

    /**
     * "Restore from backup words": `libsol.so restore --words ... --name ... --device ...` with
     * init's --files and --ui, then the node starts as usual. The words go to the node as an
     * argument only: never stored, logged or kept after the screen. A failed restore leaves no
     * half-made data directory behind, so the welcome screen still offers every choice.
     */
    private void showRestore() {
        LinearLayout col = column("Restore from backup words",
                "Type the 24 backup words you wrote down. Restoring brings back who you are, so your contacts "
                        + "keep trusting you. Your files come back by syncing from your other devices that are still online.");
        EditText words = new EditText(this);
        words.setHint("24 words, separated by spaces");
        words.setMinLines(4);
        words.setGravity(Gravity.TOP | Gravity.START);
        // Secret words: no suggestions, no learning by the keyboard, no autofill, all lowercase.
        words.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_MULTI_LINE | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        words.setImeOptions(EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING);
        words.setImportantForAutofill(View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS);
        words.setFilters(new android.text.InputFilter[]{(src, st, en, d, ds, de) -> {
            String in = src.subSequence(st, en).toString(), low = in.toLowerCase(java.util.Locale.ROOT);
            return in.equals(low) ? null : low;
        }});
        words.setTextColor(textColor());
        EditText name = new EditText(this);
        name.setHint("Your name (as your contacts know you)");
        name.setSingleLine();
        name.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_WORDS);
        name.setTextColor(textColor());
        EditText device = new EditText(this);
        device.setHint("Device name");
        device.setSingleLine();
        device.setText(Build.MODEL);
        device.setTextColor(textColor());
        Button go = new Button(this);
        go.setText("Restore");
        TextView status = new TextView(this);
        status.setTextSize(16);
        status.setPadding(0, dp(16), 0, 0);
        status.setTextColor(textColor());
        android.widget.ProgressBar spin = new android.widget.ProgressBar(this);
        spin.setVisibility(View.GONE);
        Button back = new Button(this);
        back.setText("Back");
        back.setOnClickListener(v -> showWelcome());
        go.setOnClickListener(v -> {
            String[] ws = words.getText().toString().toLowerCase(java.util.Locale.ROOT).trim().split("[\\s,]+");
            int n = ws.length == 1 && ws[0].isEmpty() ? 0 : ws.length;
            if (n != 24) { words.setError("Enter all 24 words (" + n + " so far)"); return; }
            String nm = name.getText().toString().trim();
            if (nm.isEmpty()) { name.setError("Enter your name"); return; }
            String dev = device.getText().toString().trim();
            String w = String.join(" ", ws);
            go.setEnabled(false);
            back.setEnabled(false);
            spin.setVisibility(View.VISIBLE);
            status.setText("Restoring…");
            new Thread(() -> {
                String[] r;
                if (NodeService.initialised(this)) r = new String[]{"1", "This phone already has a Sol."};
                else {
                    deleteTree(NodeService.dataDir(this)); // leftovers of an earlier failed attempt
                    NodeService.driveDir(this).mkdirs();
                    r = NodeService.cli(this, "restore", "--words", w, "--name", nm,
                            "--device", dev.isEmpty() ? Build.MODEL : dev,
                            "--files", NodeService.driveDir(this).getAbsolutePath(), "--ui", "127.0.0.1:" + NodeService.PORT);
                    if (!r[0].equals("0") || !NodeService.initialised(this)) deleteTree(NodeService.dataDir(this));
                }
                boolean ok = r[0].equals("0") && NodeService.initialised(this);
                String err = ok ? null : restoreFailure(r[1]);
                runOnUiThread(() -> {
                    spin.setVisibility(View.GONE);
                    if (!ok) {
                        status.setText(err);
                        go.setEnabled(true);
                        back.setEnabled(true);
                        return;
                    }
                    words.setText("");
                    root.removeView(setup);
                    setup = null;
                    startNode(null, null, null);
                    buildWeb();
                    handleShare(getIntent());
                    if (pendingLink != null) {
                        Uri l = Uri.parse(pendingLink);
                        pendingLink = null;
                        handleLink(l);
                    }
                });
            }, "sol-restore").start();
        });
        col.addView(words);
        col.addView(name);
        col.addView(device);
        col.addView(go);
        col.addView(spin);
        col.addView(status);
        col.addView(back);
        android.widget.ScrollView sv = new android.widget.ScrollView(this);
        sv.setFillViewport(true);
        sv.addView(col);
        show(sv);
        words.requestFocus();
    }

    /** What to tell the person when restore fails; the node's text never contains the words. */
    private static String restoreFailure(String cli) {
        String t = cli == null ? "" : cli.trim();
        String low = t.toLowerCase(java.util.Locale.ROOT);
        if (low.contains("word") || low.contains("checksum") || low.contains("mnemonic"))
            return "These words are not right. Check each word and their order against your backup, then try again.";
        return failure(t, "Restoring failed.");
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

    /** The system bar insets (status, navigation, cutout) and the keyboard's, in physical px. */
    private int insTop, insBottom, insLeft, insRight, insIme;

    /**
     * Draws the whole window under transparent status and navigation bars. Nothing is padded
     * natively except by the keyboard (so a focused field stays above it): the WebView reaches the
     * screen edges and the page is told the bar insets as the CSS custom properties
     * --sol-inset-top/-bottom/-left/-right on &lt;html&gt; (pushInsets), which the kit folds into
     * --safe-*. Android's WebView does not fill env(safe-area-inset-*) with the system bars.
     * Native setup screens get the insets as margins (padSetup).
     */
    private void edgeToEdge() {
        android.view.Window w = getWindow();
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
            padSetup();
            pushInsets();
            return ins;
        });
    }

    /** Dark bar icons on a light theme, light icons on a dark one (the system's day/night mode). */
    private void barIcons() {
        boolean night = (getResources().getConfiguration().uiMode & android.content.res.Configuration.UI_MODE_NIGHT_MASK)
                == android.content.res.Configuration.UI_MODE_NIGHT_YES;
        if (Build.VERSION.SDK_INT >= 30) {
            android.view.WindowInsetsController c = getWindow().getInsetsController();
            int both = android.view.WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                    | android.view.WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
            if (c != null) c.setSystemBarsAppearance(night ? 0 : both, both);
        } else {
            int f = root.getSystemUiVisibility() & ~(View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR);
            if (!night) f |= View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            root.setSystemUiVisibility(f);
        }
    }

    @Override
    public void onConfigurationChanged(android.content.res.Configuration c) {
        super.onConfigurationChanged(c);
        root.setBackgroundResource(R.color.win_bg);
        barIcons();
    }

    /** Keeps a native setup screen clear of the bars (the WebView is not padded; the page is). */
    private void padSetup() {
        if (setup == null) return;
        FrameLayout.LayoutParams lp = (FrameLayout.LayoutParams) setup.getLayoutParams();
        if (lp == null) return;
        lp.setMargins(insLeft, insTop, insRight, insBottom);
        setup.setLayoutParams(lp);
    }

    /**
     * Tells the current page the bar insets in CSS px: --sol-inset-* on &lt;html&gt;, which
     * ui/kit/tokens.css folds into --safe-* (max with env()). Run on every page commit and finish
     * and whenever the insets change, on every local origin (the launcher and each app).
     */
    private void pushInsets() {
        if (web == null) return;
        String u = web.getUrl();
        if (u == null || !isLocal(Uri.parse(u))) return;
        float d = getResources().getDisplayMetrics().density;
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
        // " SolAndroid/<version>" lets the node name this session "Sol app on Android".
        s.setUserAgentString(s.getUserAgentString() + " SolAndroid/" + versionName());
        web.addJavascriptInterface(new SolBridge(), "SolAndroid");
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
                    startActivityForResult(p.createIntent(), REQ_FILE);
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

    // ---- the page's bridge ----

    /** The URL of the top-level page, kept for the bridge (which runs off the UI thread). */
    private volatile String pageUrl;

    private String versionName() {
        try {
            String v = getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
            return v == null ? "0" : v;
        } catch (android.content.pm.PackageManager.NameNotFoundException e) {
            return "0";
        }
    }

    /**
     * window.SolAndroid: what a Sol page may ask of the shell. Every method answers only while the
     * top-level page is a local Sol origin (the launcher or an app on *.localhost:7777); the
     * WebView never shows other sites itself, but an embedded frame could call it.
     */
    private final class SolBridge {
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
            runOnUiThread(() -> openNative(u.toString(), mime, name));
            return true;
        }

        /**
         * Settings, "Remove Sol from this phone": after a native confirmation, erases the node's
         * data and SolDrive and returns to the welcome screen (Start new, Add to my Sol, Restore).
         */
        @android.webkit.JavascriptInterface
        public void reset() {
            if (local()) runOnUiThread(MainActivity.this::confirmRemove);
        }
    }

    /** The native second step of "Remove Sol from this phone" (a page alone can never wipe Sol). */
    private void confirmRemove() {
        new android.app.AlertDialog.Builder(this)
                .setTitle("Remove Sol from this phone?")
                .setMessage("Sol's keys and the files stored on this phone are erased. Have your 24 backup words ready to restore.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Remove", (d, w) -> removeSol())
                .show();
    }

    private void removeSol() {
        if (web != null) {
            root.removeView(web);
            web.destroy();
            web = null;
        }
        loaded = false;
        pageUrl = null;
        CookieManager.getInstance().removeAllCookies(null);
        android.webkit.WebStorage.getInstance().deleteAllData(); // the pages' cached state
        new Thread(() -> {
            stopService(new Intent(this, NodeService.class));
            for (int i = 0; i < 100 && NodeService.isAlive(); i++) {
                try { Thread.sleep(100); } catch (InterruptedException e) { return; }
            }
            deleteTree(NodeService.dataDir(this));
            deleteTree(NodeService.driveDir(this));
            deleteTree(new File(getCacheDir(), "open"));
            getSharedPreferences("sol", MODE_PRIVATE).edit().clear().apply();
            runOnUiThread(this::showWelcome);
        }, "sol-remove").start();
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
                    f = OpenProvider.file(this, key, n);
                    if (!f.exists() || (c.getContentLengthLong() >= 0 && f.length() != c.getContentLengthLong())) {
                        OpenProvider.prune(this);
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
                runOnUiThread(() -> {
                    Intent v = new Intent(Intent.ACTION_VIEW).setDataAndType(content, type)
                            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    try { startActivity(v); }
                    catch (android.content.ActivityNotFoundException e) { toast("No app on this phone can open this file"); }
                });
            } catch (Exception e) {
                runOnUiThread(() -> Toast.makeText(this, "Could not open the file", Toast.LENGTH_SHORT).show());
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
