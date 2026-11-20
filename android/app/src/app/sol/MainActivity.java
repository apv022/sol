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
        @Override public void onReceive(Context c, Intent i) { showWeb(); }
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
        if (!NodeService.initialised(this) && (name == null || !debuggable)) {
            // A debug build accepts `--es name X` so tests can skip the form.
            name = null;
            showSetup();
            return;
        }
        startNode(name);
        buildWeb();
        handleShare(in);
        if (NodeService.isReady()) showWeb();
    }

    private void startNode(String name) {
        Intent s = new Intent(this, NodeService.class);
        if (name != null) s.putExtra(NodeService.EXTRA_NAME, name);
        startForegroundService(s);
    }

    // ---- first run ----

    private void showSetup() {
        LinearLayout col = new LinearLayout(this);
        col.setOrientation(LinearLayout.VERTICAL);
        col.setGravity(Gravity.CENTER_VERTICAL);
        int p = dp(28);
        col.setPadding(p, p, p, p);
        TextView title = new TextView(this);
        title.setText("Welcome to Sol");
        title.setTextSize(30);
        title.setTextColor(textColor());
        TextView sub = new TextView(this);
        sub.setText("What should your devices and the people you share with call you?");
        sub.setTextSize(16);
        sub.setPadding(0, dp(8), 0, dp(24));
        sub.setTextColor(textColor() & 0x00ffffff | 0xb0000000);
        EditText name = new EditText(this);
        name.setHint("Your name");
        name.setSingleLine();
        name.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_WORDS);
        name.setImeOptions(EditorInfo.IME_ACTION_DONE);
        name.setTextColor(textColor());
        Button go = new Button(this);
        go.setText("Continue");
        Runnable submit = () -> {
            String n = name.getText().toString().trim();
            if (n.isEmpty()) { name.setError("Enter a name"); return; }
            root.removeView(setup);
            setup = null;
            startNode(n);
            buildWeb();
            handleShare(getIntent());
        };
        go.setOnClickListener(v -> submit.run());
        name.setOnEditorActionListener((v, a, e) -> { submit.run(); return true; });
        col.addView(title);
        col.addView(sub);
        col.addView(name);
        col.addView(go);
        setup = col;
        root.addView(col, new FrameLayout.LayoutParams(-1, -1));
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
                if (isLocal(u)) return false; // launcher and app origins stay in this WebView
                try { startActivity(new Intent(Intent.ACTION_VIEW, u)); } catch (RuntimeException ignored) { }
                return true;
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

    /** Shows the UI once the node is serving. */
    private void showWeb() {
        if (web == null) return;
        String url = pendingUrl != null ? pendingUrl : HOME;
        pendingUrl = null;
        if (!loaded || !url.equals(HOME)) web.loadUrl(url);
        loaded = true;
        web.setVisibility(View.VISIBLE);
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
        String name = URLUtil.guessFileName(url, disposition, mime);
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
                if (NodeService.isReady() && web != null) {
                    String url = pendingUrl;
                    pendingUrl = null;
                    web.loadUrl(url);
                }
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
