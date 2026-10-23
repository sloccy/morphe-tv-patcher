package com.sloccy.bootadb;

import android.app.Activity;
import android.os.Bundle;
import android.widget.Toast;

/** No screen. Opening it once (tv-patcher does this) lets the app receive boot broadcasts. */
public final class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        boolean on = Adb.ensureOn(this);
        Adb.schedule(this);
        Toast.makeText(this, on ? "Wireless debugging is on" : "Wireless debugging helper: permission missing", Toast.LENGTH_LONG).show();
        finish();
    }
}
