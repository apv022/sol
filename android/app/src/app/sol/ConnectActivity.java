package app.sol;

import android.app.Activity;
import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageInfo;
import android.content.pm.PackageManager;
import android.content.pm.Signature;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Typeface;
import android.graphics.drawable.AdaptiveIconDrawable;
import android.graphics.drawable.Drawable;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.util.Base64;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.widget.Button;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.List;

/**
 * Another Android app asking to become a Sol app (design/APPS_EVERYWHERE.md M5; the contract
 * for app developers is android/README.md "For Android developers").
 *
 * The caller starts {@code app.sol.action.CONNECT} with startActivityForResult, so Android tells
 * us who it is (getCallingPackage; a plain startActivity has no caller and is refused). Who it
 * is comes only from PackageManager (its label, icon and signing certificate), never from what
 * it claims in the extras, so an app cannot pass itself off as another on the approval screen.
 * On Allow the node registers it (`sol app connect`, the request on stdin, never argv or
 * logcat) bound to the package and the SHA-256 of its original signing certificate, and the
 * caller gets RESULT_OK with extras token, api and app_id. Anything else is RESULT_CANCELED
 * with an "error" extra (denied, not_for_result, not_set_up, bad_request, failed). The token
 * is never logged or kept here: the node stores only its hash. The app's launcher icon, also
 * from PackageManager, goes with the request as a 192 px PNG (icon_png) so Sol's Apps grid can
 * show it; reconnecting refreshes it.
 */
public class ConnectActivity extends Activity {
    /** The intent action third-party apps send. */
    public static final String ACTION = "app.sol.action.CONNECT";

    private final Handler main = new Handler(Looper.getMainLooper());
    private String caller, cert, appId;
    private JSONObject manifest;
    private Button allow, deny;
    private TextView status;
    private boolean answered;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        // No other app's window may cover the approval (tapjacking); the screen shows no
        // secret, so screenshots stay allowed. Older Androids get the touch filter only.
        if (Build.VERSION.SDK_INT >= 31) getWindow().setHideOverlayWindows(true);
        caller = getCallingPackage();
        if (caller == null) { fail("not_for_result", null); return; }
        if (caller.equals(getPackageName())) { fail("bad_request", null); return; }
        try {
            cert = certOf(caller);
            manifest = manifestOf(getIntent());
            appId = manifest.getString("id");
        } catch (Exception e) {
            fail("bad_request", e.getMessage());
            return;
        }
        if (!NodeService.initialised(this)) {
            show(getString(R.string.connect_not_set_up), null, "not_set_up");
            return;
        }
        show(null, null, null);
    }

    /** The size of the icon sent to the node: the Apps grid shows it at up to 60 CSS px. */
    static final int ICON_PX = 192;

    /**
     * The package's launcher icon from PackageManager as a base64 PNG of ICON_PX square, or null.
     * An adaptive icon is drawn full bleed (its background and foreground layers, without the
     * launcher's mask), so the grid's own rounded square shapes it like every other Sol app.
     */
    private String iconPng(String pkg) {
        try {
            Drawable d = getPackageManager().getApplicationIcon(pkg);
            Bitmap bm = Bitmap.createBitmap(ICON_PX, ICON_PX, Bitmap.Config.ARGB_8888);
            Canvas c = new Canvas(bm);
            if (Build.VERSION.SDK_INT >= 26 && d instanceof AdaptiveIconDrawable) {
                // The layers are 108 dp, of which the middle 72 dp is the icon: inset by 1/4 of
                // the visible size on every side (AdaptiveIconDrawable.EXTRA_INSET_PERCENTAGE).
                int in = ICON_PX / 4;
                AdaptiveIconDrawable a = (AdaptiveIconDrawable) d;
                for (Drawable layer : new Drawable[]{a.getBackground(), a.getForeground()}) {
                    if (layer == null) continue;
                    layer.setBounds(-in, -in, ICON_PX + in, ICON_PX + in);
                    layer.draw(c);
                }
            } else {
                d.setBounds(0, 0, ICON_PX, ICON_PX);
                d.draw(c);
            }
            ByteArrayOutputStream out = new ByteArrayOutputStream();
            if (!bm.compress(Bitmap.CompressFormat.PNG, 100, out)) return null;
            bm.recycle();
            // The node refuses icons over 256 KB; a 192 px PNG is far smaller.
            return out.size() > 256 * 1024 ? null : Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP);
        } catch (Exception e) {
            return null; // no icon: the grid shows a glyph
        }
    }

    /** The SHA-256 of the package's original signing certificate (rotation keeps it). */
    private String certOf(String pkg) throws PackageManager.NameNotFoundException, NoSuchAlgorithmException {
        PackageManager pm = getPackageManager();
        Signature[] sigs;
        if (Build.VERSION.SDK_INT >= 28) {
            PackageInfo pi = pm.getPackageInfo(pkg, PackageManager.GET_SIGNING_CERTIFICATES);
            if (pi.signingInfo == null || pi.signingInfo.hasMultipleSigners())
                throw new IllegalArgumentException("apps signed by several keys cannot connect");
            sigs = pi.signingInfo.getSigningCertificateHistory();
        } else {
            sigs = pm.getPackageInfo(pkg, PackageManager.GET_SIGNATURES).signatures;
            if (sigs != null && sigs.length > 1) throw new IllegalArgumentException("apps signed by several keys cannot connect");
        }
        if (sigs == null || sigs.length == 0) throw new IllegalArgumentException("unsigned app");
        byte[] d = MessageDigest.getInstance("SHA-256").digest(sigs[0].toByteArray());
        StringBuilder sb = new StringBuilder();
        for (byte x : d) sb.append(String.format("%02x", x));
        return sb.toString();
    }

    /** The manifest the caller asked for: its "manifest" extra, or one built from the others. */
    static JSONObject manifestOf(Intent in) throws JSONException {
        String raw = in.getStringExtra("manifest");
        JSONObject m = raw != null ? new JSONObject(raw) : new JSONObject();
        if (raw == null) {
            m.put("id", in.getStringExtra("app_id"));
            m.put("name", in.getStringExtra("name"));
            if (in.getStringExtra("description") != null) m.put("description", in.getStringExtra("description"));
            JSONArray sc = new JSONArray();
            String[] scopes = in.getStringArrayExtra("scopes");
            if (scopes != null) for (String s : scopes) sc.put(s);
            m.put("scopes", sc);
        }
        String id = m.optString("id", "");
        if (id.isEmpty()) throw new JSONException("app_id is required");
        if (in.getStringExtra("app_id") != null && !id.equals(in.getStringExtra("app_id")))
            throw new JSONException("app_id and the manifest's id differ");
        if (m.optString("name", "").isEmpty()) m.put("name", id);
        return m;
    }

    /** A scope in plain words, as the approval screen shows it. */
    private String scopeWords(String s) {
        switch (s) {
            case "people": return getString(R.string.scope_people);
            case "feeds": return getString(R.string.scope_feeds);
            case "blobs": return getString(R.string.scope_blobs);
            case "actions": return getString(R.string.scope_actions);
            case "inbox": return getString(R.string.scope_inbox);
            case "dial": return getString(R.string.scope_dial);
            default: return s;
        }
    }

    /**
     * Draws the approval screen (message == null), or a message with an OK button that answers
     * the caller with error okCode.
     */
    private void show(String message, String detail, String okCode) {
        PackageManager pm = getPackageManager();
        CharSequence label = caller;
        Drawable icon = null;
        try {
            ApplicationInfo ai = pm.getApplicationInfo(caller, 0);
            label = pm.getApplicationLabel(ai);
            icon = pm.getApplicationIcon(ai);
        } catch (PackageManager.NameNotFoundException ignored) { }

        int pad = dp(24);
        LinearLayout col = new LinearLayout(this);
        col.setOrientation(LinearLayout.VERTICAL);
        col.setPadding(pad, dp(56), pad, pad);
        if (icon != null) {
            ImageView iv = new ImageView(this);
            iv.setImageDrawable(icon);
            col.addView(iv, new LinearLayout.LayoutParams(dp(64), dp(64)));
        }
        if (message != null) {
            col.addView(text(message, 20, true));
            if (detail != null) col.addView(text(detail, 15, false));
        } else {
            col.addView(text(getString(R.string.connect_title, label), 22, true));
            String name = manifest.optString("name", appId);
            col.addView(text(getString(R.string.connect_as, name, appId, caller), 14, false));
            List<String> lines = new ArrayList<>();
            JSONArray sc = manifest.optJSONArray("scopes");
            for (int i = 0; sc != null && i < sc.length(); i++) lines.add(scopeWords(sc.optString(i)));
            JSONArray reads = manifest.optJSONArray("reads");
            for (int i = 0; reads != null && i < reads.length(); i++) lines.add(getString(R.string.scope_reads, reads.optString(i)));
            if (lines.isEmpty()) lines.add(getString(R.string.scope_none));
            col.addView(text(getString(R.string.connect_it_may), 16, true));
            for (String l : lines) col.addView(text("•  " + l, 16, false));
            col.addView(text(getString(R.string.connect_revoke, appId), 14, false));
        }
        status = text("", 14, false);
        col.addView(status);

        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.END);
        row.setPadding(0, dp(16), 0, 0);
        if (message != null) {
            Button ok = new Button(this);
            ok.setText(android.R.string.ok);
            ok.setOnClickListener(v -> fail(okCode, detail));
            row.addView(ok);
        } else {
            deny = new Button(this);
            deny.setText(R.string.connect_deny);
            deny.setOnClickListener(v -> fail("denied", null));
            allow = new Button(this);
            allow.setText(R.string.connect_allow);
            allow.setFilterTouchesWhenObscured(true); // no tapjacking through an overlay
            allow.setOnClickListener(v -> approve());
            row.addView(deny);
            row.addView(allow);
        }
        col.addView(row);
        ScrollView sv = new ScrollView(this);
        sv.setFilterTouchesWhenObscured(true);
        sv.addView(col);
        setContentView(sv);
    }

    private TextView text(String s, int sp, boolean bold) {
        TextView t = new TextView(this);
        t.setText(s);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, sp);
        if (bold) t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setPadding(0, dp(12), 0, 0);
        return t;
    }

    private int dp(int v) { return Math.round(v * getResources().getDisplayMetrics().density); }

    /** Allow: start the node if needed, register the app, hand the token back. */
    private void approve() {
        allow.setEnabled(false);
        deny.setEnabled(false);
        status.setText(R.string.connect_working);
        startForegroundService(new Intent(this, NodeService.class));
        final String req;
        try {
            JSONObject o = new JSONObject().put("manifest", manifest).put("package", caller).put("cert", cert);
            String icon = iconPng(caller);
            if (icon != null) o.put("icon_png", icon);
            req = o.toString();
        } catch (JSONException e) { fail("bad_request", e.getMessage()); return; }
        new Thread(() -> {
            for (int i = 0; i < 300 && !NodeService.isReady(); i++) {
                try { Thread.sleep(100); } catch (InterruptedException e) { return; }
            }
            String[] r = NodeService.cli2In(this, req, "app", "connect");
            main.post(() -> done(r));
        }, "sol-connect").start();
    }

    private void done(String[] r) {
        if (!"0".equals(r[0])) {
            // The node's message is user-facing and never holds a token (it failed).
            show(getString(R.string.connect_failed), r[1].replaceFirst("^sol: ", ""), "failed");
            return;
        }
        try {
            JSONObject o = new JSONObject(r[1]);
            Intent out = new Intent()
                    .putExtra("token", o.getString("token"))
                    .putExtra("api", o.getString("api"))
                    .putExtra("app_id", o.getString("id"));
            answered = true;
            setResult(RESULT_OK, out);
            finish();
        } catch (JSONException e) {
            fail("failed", null);
        }
    }

    private void fail(String code, String detail) {
        if (answered) return;
        answered = true;
        Intent out = new Intent().putExtra("error", code);
        if (detail != null) out.putExtra("message", detail);
        setResult(RESULT_CANCELED, out);
        finish();
    }

    @Override
    public void onBackPressed() { fail("denied", null); }
}
