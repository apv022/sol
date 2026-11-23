package app.sol;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

import org.json.JSONException;
import org.json.JSONObject;

/**
 * The buttons of an inbox notification: Approve and Deny on an approval, Dismiss on a notice. Each
 * runs the same CLI command a person would (`libsol.so inbox approve|deny|dismiss <id>`) against
 * this phone's node and then removes the notification; if that fails, the notification stays and
 * says why. Not exported: only Sol's own notifications can send it.
 */
public class InboxReceiver extends BroadcastReceiver {
    static final String ACTION_APPROVE = "app.sol.INBOX_APPROVE";
    static final String ACTION_DENY = "app.sol.INBOX_DENY";
    static final String ACTION_DISMISS = "app.sol.INBOX_DISMISS";
    /** String extra: the item as JSON (its id, and what to show again if the answer fails). */
    static final String EXTRA_ITEM = "item";

    @Override
    public void onReceive(Context c, Intent in) {
        String a = in.getAction();
        String verb = ACTION_APPROVE.equals(a) ? "approve" : ACTION_DENY.equals(a) ? "deny" : ACTION_DISMISS.equals(a) ? "dismiss" : null;
        JSONObject it;
        try { it = new JSONObject(in.getStringExtra(EXTRA_ITEM)); }
        catch (JSONException | NullPointerException e) { return; }
        String id = it.optString("id", "");
        if (verb == null || id.isEmpty()) return;
        Context app = c.getApplicationContext();
        PendingResult pending = goAsync();
        new Thread(() -> {
            try {
                String[] r = NodeService.isReady() ? NodeService.cli2(app, "inbox", verb, id)
                        : new String[]{"-1", "Sol is not running."};
                if (r[0].equals("0")) {
                    Notifier.cancel(app, id);
                } else {
                    String why = r[1].startsWith("sol: ") ? r[1].substring(5) : r[1];
                    Notifier.post(app, it, "Could not " + verb + ": " + why);
                }
            } finally {
                pending.finish();
            }
        }, "sol-inbox-answer").start();
    }
}
