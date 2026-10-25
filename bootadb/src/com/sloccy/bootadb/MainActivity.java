package com.sloccy.bootadb;

import android.app.Activity;
import android.os.Bundle;

/** No screen, no messages. Opening it once (tv-patcher does this) lets the app receive boot broadcasts. */
public final class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        Adb.ensureAll(this);
        Adb.schedule(this);
        finish();
    }
}
