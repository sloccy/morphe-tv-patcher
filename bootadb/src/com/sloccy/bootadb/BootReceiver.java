package com.sloccy.bootadb;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** Runs after boot (and after the app is updated). */
public final class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        Adb.ensureAll(context);
        Adb.schedule(context);
    }
}
