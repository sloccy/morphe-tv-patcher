package com.sloccy.bootadb;

import android.app.job.JobInfo;
import android.app.job.JobScheduler;
import android.content.ComponentName;
import android.content.ContentResolver;
import android.content.Context;
import android.provider.Settings;
import android.util.Log;

/**
 * Keeps Wireless debugging on and a few privacy settings off, and keeps the hourly re-check
 * scheduled. A value is written only when it differs from what is wanted.
 */
final class Adb {
    private static final String TAG = "BootAdb";
    private static final int JOB_ID = 1;

    private static final boolean GLOBAL = true;
    private static final boolean SECURE = false;

    /** {table, key, wanted value}. */
    private static final Object[][] WANTED = {
        {GLOBAL, "adb_wifi_enabled", "1"},
        {GLOBAL, "wifi_scan_always_enabled", "0"},
        {GLOBAL, "network_recommendations_enabled", "0"},
        {GLOBAL, "upload_apk_enable", "0"},
        {GLOBAL, "package_verifier_user_consent", "-1"},
        {GLOBAL, "send_action_app_error", "0"},
        {SECURE, "location_mode", "0"},
    };

    private Adb() {}

    /** Brings every wanted setting to its value. One rejected key does not stop the others. */
    static void ensureAll(Context context) {
        ContentResolver resolver = context.getContentResolver();
        for (Object[] w : WANTED) {
            boolean global = (Boolean) w[0];
            String key = (String) w[1];
            String want = (String) w[2];
            try {
                String have = global ? Settings.Global.getString(resolver, key) : Settings.Secure.getString(resolver, key);
                if (want.equals(have)) {
                    continue;
                }
                boolean ok = global ? Settings.Global.putString(resolver, key, want) : Settings.Secure.putString(resolver, key, want);
                Log.i(TAG, key + ": " + have + " -> " + want + (ok ? "" : " (not accepted)"));
            } catch (SecurityException e) {
                Log.w(TAG, key + ": WRITE_SECURE_SETTINGS is not granted");
            } catch (RuntimeException e) {
                Log.w(TAG, key + ": " + e);
            }
        }
    }

    /** Makes sure the hourly re-check job exists (it survives reboots). */
    static void schedule(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        if (scheduler == null || scheduler.getPendingJob(JOB_ID) != null) {
            return;
        }
        scheduler.schedule(new JobInfo.Builder(JOB_ID, new ComponentName(context, KeepOnJob.class))
                .setPeriodic(60 * 60 * 1000L)
                .setPersisted(true)
                .build());
    }
}
