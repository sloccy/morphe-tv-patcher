package com.sloccy.bootadb;

import android.app.job.JobInfo;
import android.app.job.JobScheduler;
import android.content.ComponentName;
import android.content.ContentResolver;
import android.content.Context;
import android.provider.Settings;

/** Turns Wireless debugging on and keeps the periodic re-check scheduled. */
final class Adb {
    private static final String KEY = "adb_wifi_enabled";
    private static final int JOB_ID = 1;

    private Adb() {}

    /** @return true when Wireless debugging is on (or was just switched on). */
    static boolean ensureOn(Context context) {
        ContentResolver resolver = context.getContentResolver();
        try {
            if (Settings.Global.getInt(resolver, KEY, 0) == 1) {
                return true;
            }
            return Settings.Global.putInt(resolver, KEY, 1);
        } catch (SecurityException e) {
            return false; // the WRITE_SECURE_SETTINGS grant is missing
        }
    }

    /** Makes sure the 15-minute re-check job exists (it survives reboots). */
    static void schedule(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        if (scheduler == null || scheduler.getPendingJob(JOB_ID) != null) {
            return;
        }
        scheduler.schedule(new JobInfo.Builder(JOB_ID, new ComponentName(context, KeepOnJob.class))
                .setPeriodic(15 * 60 * 1000L)
                .setPersisted(true)
                .build());
    }
}
