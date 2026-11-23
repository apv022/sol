package app.sol;

import android.app.Activity;
import android.app.ActivityManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.net.Uri;
import android.os.Bundle;
import android.widget.FrameLayout;

/**
 * One Sol app (Files, Notes, ...) full screen in a task of its own: a separate entry in recents
 * with the app's name and icon, no Sol tab bar, and Back at the app's start goes to the home
 * screen. The intent's data is sol-app://&lt;id&gt; and the manifest's documentLaunchMode
 * "intoExisting" keeps one task per app id: opening Files again brings back its task.
 *
 * The window is the same as the Sol app's (SolWeb: edge to edge, signed in through the shared
 * session, downloads, file picker, window.SolAndroid). If the node is not running yet it is
 * started and the page loads once it serves; before Sol is set up the Sol app opens instead.
 */
public class AppActivity extends Activity {
    /** String extra: the app's display name (the task label until the app list is known). */
    static final String EXTRA_NAME = "name";

    private SolWeb sw;
    private String id;
    private boolean shown;
    private final BroadcastReceiver readyReceiver = new BroadcastReceiver() {
        @Override public void onReceive(Context c, Intent i) { showApp(); }
    };

    /** The intent that opens app id in its own task (name may be null). */
    static Intent intent(Context c, String id, String name) {
        Intent i = new Intent(c, AppActivity.class).setAction(Intent.ACTION_VIEW)
                .setData(Uri.parse("sol-app://" + id))
                .addFlags(Intent.FLAG_ACTIVITY_NEW_DOCUMENT | Intent.FLAG_ACTIVITY_NEW_TASK);
        if (name != null) i.putExtra(EXTRA_NAME, name);
        return i;
    }

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        id = Shortcuts.idOf(getIntent().getData());
        if (id == null || !NodeService.initialised(this)) {
            // Not set up yet (or a broken shortcut): the Sol app knows what to do.
            startActivity(new Intent(this, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
            finishAndRemoveTask();
            return;
        }
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundResource(R.color.win_bg);
        setContentView(root);
        sw = new SolWeb(this, root);
        sw.edgeToEdge();
        sw.build();
        String name = Shortcuts.appName(this, id);
        if (name == null) name = getIntent().getStringExtra(EXTRA_NAME);
        label(name == null ? id : name);
        registerReceiver(readyReceiver, new IntentFilter(NodeService.ACTION_READY), Context.RECEIVER_NOT_EXPORTED);
        startForegroundService(new Intent(this, NodeService.class));
        if (NodeService.isReady()) showApp();
    }

    /** The task's label and icon in recents: the app's, not Sol's. */
    @SuppressWarnings("deprecation")
    private void label(String name) {
        Context app = getApplicationContext();
        new Thread(() -> {
            android.graphics.Bitmap icon = Shortcuts.icon(app, id, name);
            runOnUiThread(() -> {
                if (isDestroyed()) return;
                setTitle(name);
                setTaskDescription(new ActivityManager.TaskDescription(name, icon, getColor(R.color.win_bg)));
            });
        }, "sol-task-icon").start();
    }

    private void showApp() {
        if (shown || sw == null || sw.web == null) return;
        shown = true;
        sw.show(SolWeb.appUrl(id));
    }

    @Override
    protected void onNewIntent(Intent in) {
        super.onNewIntent(in); // the same app again (intoExisting): its task comes back as it was
    }

    @Override
    public void onConfigurationChanged(android.content.res.Configuration c) {
        super.onConfigurationChanged(c);
        if (sw != null) sw.configurationChanged();
    }

    @Override
    public void onBackPressed() {
        // At the app's start Back leaves to the home screen; the task stays in recents.
        if (!sw.back()) moveTaskToBack(true);
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        sw.activityResult(req, res, data);
    }

    @Override
    public void onRequestPermissionsResult(int req, String[] perms, int[] grants) {
        if (sw != null) sw.permissionsResult(req, grants);
    }

    @Override
    protected void onDestroy() {
        if (sw != null) {
            unregisterReceiver(readyReceiver);
            sw.destroy();
        }
        super.onDestroy();
    }
}
