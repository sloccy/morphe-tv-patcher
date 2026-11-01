package com.sloccy.bootadb;

import android.app.Activity;
import android.os.Bundle;

/**
 * No screen, no messages. This is the one component tv-patcher can trigger on demand, so it is
 * what applies the settings: starting it also takes the app out of Android's "stopped" state, so
 * it can receive the boot broadcast afterwards.
 *
 * <p>It is exported, but only to a caller holding WRITE_SECURE_SETTINGS (see the manifest): the adb
 * shell has it and no app gets it without an adb grant. It cannot simply be left not exported,
 * because some TVs refuse the shell's {@code am start} of a non-exported activity ("not exported
 * from uid"), and then the helper never runs at all.
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
