package com.sloccy.bootadb;

import android.app.Activity;
import android.os.Bundle;

/**
 * No screen, no messages. Opening it once (tv-patcher does this) lets the app receive boot
 * broadcasts. The activity is exported, so any app on the TV can start it: it does nothing
 * privileged, only schedules the app's own hourly job. The settings are applied by BootReceiver
 * and by that job; tv-patcher runs the job once after installing the app.
 */
public final class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        Adb.schedule(this);
        finish();
    }
}
