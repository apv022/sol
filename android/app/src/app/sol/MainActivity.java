package app.sol;

import android.Manifest;
import android.app.Activity;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.ApplicationInfo;
import android.database.Cursor;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.provider.OpenableColumns;
import android.text.InputType;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.inputmethod.EditorInfo;
import android.webkit.CookieManager;
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
import java.util.ArrayList;

/**
 * The Sol app: a first-run screen (start new, add to my Sol, restore), then Sol's own UI in a
 * WebView (SolWeb: the launcher at http://localhost:7777/, apps at http://&lt;app&gt;.localhost:7777/,
 * edge to edge, signed in, window.SolAndroid). Shared files are copied into
 * SolDrive/Inbox-from-share and Files is opened. An app opened from a launcher shortcut or a
 * notification runs in its own task instead (AppActivity).
 */
public class MainActivity extends Activity {
    private static final int REQ_NOTIF = 2;
    /** Boolean extra: open Sol to ask for the Wi-Fi Direct permission (WifiDirect's notification). */
    static final String EXTRA_WIFI_DIRECT_ASK = "wifi_direct_ask";
    /** Boolean extra: open Sol to ask for the Bluetooth permissions (Ble's notification). */
    static final String EXTRA_BLE_ASK = "ble_ask";
    /** String extra: a local Sol URL to open (a notification's "Sol inbox"). */
    static final String EXTRA_URL = "url";

    private FrameLayout root;
    private SolWeb sw;
    private View setup;
    private String pendingUrl;
    private final BroadcastReceiver readyReceiver = new BroadcastReceiver() {
        @Override public void onReceive(Context c, Intent i) { if (!joinPending() && setup == null) showWeb(); }
    };

    @Override
    protected void onCreate(android.os.Bundle b) {
        super.onCreate(b);
        root = new FrameLayout(this);
        root.setBackgroundResource(R.color.win_bg);
        setContentView(root);
        sw = new SolWeb(this, root);
        sw.onInsets = this::padSetup;
        sw.onReset = this::confirmRemove;
        sw.edgeToEdge();
        if (Build.VERSION.SDK_INT >= 33) requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIF);
        registerReceiver(readyReceiver, new IntentFilter(NodeService.ACTION_READY), Context.RECEIVER_NOT_EXPORTED);
        start(getIntent());
        debugCalendarSync(getIntent());
    }

    /**
     * A debug build accepts `--ez calendar_sync true|false` so tests can flip calendar sync
     * without the page (the same path as window.SolAndroid.calendarSync).
     */
    private void debugCalendarSync(Intent in) {
        if (in.getBooleanExtra(EXTRA_WIFI_DIRECT_ASK, false)) { // the permission notification
            in.removeExtra(EXTRA_WIFI_DIRECT_ASK);
            sw.setWifiDirect(true);
        }
        if (in.getBooleanExtra(EXTRA_BLE_ASK, false)) { // the Bluetooth permission notification
            in.removeExtra(EXTRA_BLE_ASK);
            sw.setBle(true);
        }
        // A debug build also takes `--ez ble true|false` (the SolAndroid.ble path).
        if ((getApplicationInfo().flags & ApplicationInfo.FLAG_DEBUGGABLE) != 0 && in.hasExtra("ble")) {
            boolean on = in.getBooleanExtra("ble", false);
            in.removeExtra("ble");
            sw.setBle(on);
        }
        // A debug build also takes `--ez wifi_direct true|false` (the SolAndroid.wifiDirect path).
        if ((getApplicationInfo().flags & ApplicationInfo.FLAG_DEBUGGABLE) != 0 && in.hasExtra("wifi_direct")) {
            boolean on = in.getBooleanExtra("wifi_direct", false);
            in.removeExtra("wifi_direct");
            sw.setWifiDirect(on);
        }
        if ((getApplicationInfo().flags & ApplicationInfo.FLAG_DEBUGGABLE) == 0 || !in.hasExtra("calendar_sync")) return;
        boolean on = in.getBooleanExtra("calendar_sync", false);
        in.removeExtra("calendar_sync");
        sw.setCalendarSync(on);
    }

    @Override
    protected void onNewIntent(Intent in) {
        super.onNewIntent(in);
        setIntent(in);
        debugCalendarSync(in);
        Uri link = solLink(in);
        if (link != null) {
            if (!NodeService.initialised(this) || joinPending()) start(in);
            else { buildWeb(); handleLink(link); }
            return;
        }
        if (takeUrl(in) && NodeService.initialised(this) && !joinPending() && sw.web != null && NodeService.isReady()) {
            showWeb();
            return;
        }
        handleShare(in);
    }

    /** Takes a local URL to open from the intent (a notification's "Sol inbox"); true if there was one. */
    private boolean takeUrl(Intent in) {
        String u = in.getStringExtra(EXTRA_URL);
        if (u == null || !SolWeb.isLocal(Uri.parse(u))) return false;
        in.removeExtra(EXTRA_URL);
        pendingUrl = u;
        return true;
    }

    @Override
    protected void onDestroy() {
        unregisterReceiver(readyReceiver);
        sw.destroy();
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
        takeUrl(in);
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

    // ---- window and web view (SolWeb) ----

    @Override
    public void onConfigurationChanged(android.content.res.Configuration c) {
        super.onConfigurationChanged(c);
        sw.configurationChanged();
    }

    /** Keeps a native setup screen clear of the bars (the WebView is not padded; the page is). */
    private void padSetup() {
        if (setup == null) return;
        FrameLayout.LayoutParams lp = (FrameLayout.LayoutParams) setup.getLayoutParams();
        if (lp == null) return;
        lp.setMargins(sw.insLeft, sw.insTop, sw.insRight, sw.insBottom);
        setup.setLayoutParams(lp);
    }

    private void buildWeb() { sw.build(); }

    /** Shows the UI once the node is serving: where a share or notification pointed, else home. */
    private void showWeb() {
        if (sw.web == null) return;
        String target = pendingUrl != null ? pendingUrl : SolWeb.HOME;
        pendingUrl = null;
        sw.show(target);
    }

    @Override
    public void onBackPressed() {
        if (!sw.back()) super.onBackPressed();
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        sw.activityResult(req, res, data);
    }

    @Override
    public void onRequestPermissionsResult(int req, String[] perms, int[] grants) {
        if (sw != null) sw.permissionsResult(req, grants);
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
        sw.destroy();
        CookieManager.getInstance().removeAllCookies(null);
        android.webkit.WebStorage.getInstance().deleteAllData(); // the pages' cached state
        Shortcuts.clear(this);
        new Thread(() -> {
            stopService(new Intent(this, NodeService.class));
            for (int i = 0; i < 100 && NodeService.isAlive(); i++) {
                try { Thread.sleep(100); } catch (InterruptedException e) { return; }
            }
            deleteTree(NodeService.dataDir(this));
            deleteTree(NodeService.driveDir(this));
            deleteTree(new File(getCacheDir(), "open"));
            Shortcuts.clearIcons(this);
            CalendarSync.disable(getApplicationContext()); // Sol's calendars leave the phone's calendar apps
            getSharedPreferences("sol", MODE_PRIVATE).edit().clear().apply();
            getSharedPreferences(Notifier.PREFS, MODE_PRIVATE).edit().clear().apply();
            runOnUiThread(this::showWelcome);
        }, "sol-remove").start();
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
        pendingUrl = SolWeb.appUrl("files");
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
                if (NodeService.isReady() && sw.web != null) showWeb();
            });
        }, "sol-share").start();
    }

    private String displayName(Uri u) {
        try (Cursor c = getContentResolver().query(u, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst() && !c.isNull(0)) return SolWeb.sanitize(c.getString(0));
        } catch (RuntimeException ignored) { }
        String last = u.getLastPathSegment();
        return SolWeb.sanitize(last == null ? "shared" : last);
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
