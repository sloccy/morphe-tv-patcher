package com.sloccy.bootadb;

import android.app.Activity;
import android.os.Bundle;

/**
 * No screen, no messages. This is the one component tv-patcher can trigger on demand, so it is
 * what applies the settings: starting it also takes the app out of Android's "stopped" state, so
 * it can receive the boot broadcast afterwards.
 *
 * <p>It used to only schedule the jobs, because an exported, launchable activity that any app on
 * the TV could start had no business doing privileged work. That left the settings to BootReceiver
 * and to the two jobs, none of which had ever run on a TV that was not restarted — so the helper
 * did nothing at all. The activity is no longer exported or launchable instead: the shell can still
 * start it by name (tv-patcher does), and nothing on the TV can.
 */
public final class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        Adb.ensureAll(this); // first: scheduling must never be what decides whether this happens
        Adb.schedule(this);
        finish();
    }
}
