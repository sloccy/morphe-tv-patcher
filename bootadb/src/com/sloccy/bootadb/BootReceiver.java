package com.sloccy.bootadb;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** Runs after boot (and after the app is updated). */
public final class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        // Exported for the system's boot broadcasts, so act only on the actions we registered for.
        String action = intent == null ? null : intent.getAction();
        if (!Intent.ACTION_BOOT_COMPLETED.equals(action)
                && !Intent.ACTION_LOCKED_BOOT_COMPLETED.equals(action)
                && !Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)) {
            return;
        }
        Adb.ensureAll(context);
        Adb.schedule(context);
    }
}
