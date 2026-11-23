package app.sol;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;

/**
 * The target of every launcher shortcut (sol-app://&lt;id&gt;): it opens the app's own task and
 * finishes at once, without a window.
 *
 * It exists because the launcher starts a shortcut with FLAG_ACTIVITY_CLEAR_TASK, which would
 * restart an app already open in its task and lose its place. This trampoline runs in a task of
 * its own (excluded from recents) and hands over to AppActivity, whose documentLaunchMode
 * "intoExisting" brings back the app's task as it was.
 */
public class ShortcutActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        Intent in = getIntent();
        String id = Shortcuts.idOf(in.getData());
        if (id != null) {
            try { startActivity(AppActivity.intent(this, id, in.getStringExtra(AppActivity.EXTRA_NAME))); }
            catch (RuntimeException ignored) { }
        }
        finish();
    }
}
