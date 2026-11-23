package app.sol.testclient;

import android.app.Activity;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.SharedPreferences;
import android.os.Bundle;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.TextView;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * The smallest third-party Sol app: "Connect to Sol" sends app.sol.action.CONNECT for result,
 * keeps the token it gets back, and calls GET /v1/whoami with it. Test only (android/testclient,
 * never in a release). A real app would keep the token in its own private storage, as this does.
 */
public class MainActivity extends Activity {
    private static final int CONNECT = 1;
    private TextView out;
    private SharedPreferences prefs;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        prefs = getSharedPreferences("sol", MODE_PRIVATE);
        LinearLayout col = new LinearLayout(this);
        col.setOrientation(LinearLayout.VERTICAL);
        col.setPadding(48, 96, 48, 48);
        Button connect = new Button(this);
        connect.setText("Connect to Sol");
        connect.setOnClickListener(v -> {
            Intent i = new Intent("app.sol.action.CONNECT")
                    .putExtra("app_id", "testclient")
                    .putExtra("name", "Test client")
                    .putExtra("description", "Proves the Sol connect handshake.")
                    .putExtra("scopes", new String[]{"people"});
            try { startActivityForResult(i, CONNECT); }
            catch (ActivityNotFoundException e) { out.setText("Sol is not installed"); }
        });
        Button whoami = new Button(this);
        whoami.setText("Call whoami");
        whoami.setOnClickListener(v -> whoami());
        out = new TextView(this);
        out.setTextSize(16);
        out.setPadding(0, 32, 0, 0);
        col.addView(connect);
        col.addView(whoami);
        col.addView(out);
        setContentView(col);
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        if (req != CONNECT) return;
        if (res != RESULT_OK || data == null) {
            out.setText("Not connected: " + (data == null ? "canceled" : data.getStringExtra("error")
                    + (data.getStringExtra("message") != null ? " (" + data.getStringExtra("message") + ")" : "")));
            return;
        }
        prefs.edit().putString("token", data.getStringExtra("token")).putString("api", data.getStringExtra("api")).apply();
        out.setText("Connected as " + data.getStringExtra("app_id") + " at " + data.getStringExtra("api"));
        whoami();
    }

    private void whoami() {
        String api = prefs.getString("api", null), token = prefs.getString("token", null);
        if (api == null) { out.setText("Not connected yet"); return; }
        new Thread(() -> {
            String text;
            try {
                HttpURLConnection c = (HttpURLConnection) new URL(api + "/v1/whoami").openConnection();
                c.setRequestProperty("Authorization", "Bearer " + token);
                int code = c.getResponseCode();
                InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
                ByteArrayOutputStream buf = new ByteArrayOutputStream();
                byte[] b = new byte[4096];
                for (int n; in != null && (n = in.read(b)) > 0; ) buf.write(b, 0, n);
                text = "GET /v1/whoami: " + code + "\n" + buf.toString("UTF-8");
            } catch (Exception e) {
                text = "GET /v1/whoami failed: " + e;
            }
            String t = text;
            runOnUiThread(() -> out.setText(t));
        }).start();
    }
}
