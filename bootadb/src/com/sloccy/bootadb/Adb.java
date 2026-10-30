package com.sloccy.bootadb;

import android.app.job.JobInfo;
import android.app.job.JobScheduler;
import android.content.ComponentName;
import android.content.ContentResolver;
import android.content.Context;
import android.os.SystemClock;
import android.provider.Settings;
import android.util.Log;

/**
 * Keeps Wireless debugging on and a few privacy settings off, and keeps the re-checks scheduled.
 * A value is written only when it differs from what is wanted.
 */
final class Adb {
    private static final String TAG = "BootAdb";
    private static final int JOB_ID = 1;
    private static final int JOB_ID_WATCH = 2;

    /** How often the backstop job re-checks. 15 minutes is JobScheduler's floor. */
    private static final long EVERY = 15 * 60 * 1000L;

    private static final boolean GLOBAL = true;
    private static final boolean SECURE = false;

    static final String ADB_WIFI = "adb_wifi_enabled";

    /** {table, key, wanted value}. */
    private static final Object[][] WANTED = {
        {GLOBAL, ADB_WIFI, "1"},
        // Android deletes an ADB key that has not connected for adb_allowed_connection_time
        // (7 days by default), which unpairs this server from a TV it simply had no work for.
        // 0 means the keys never expire.
        {GLOBAL, "adb_allowed_connection_time", "0"},
        // Developer options off takes Wireless debugging with it.
        {GLOBAL, "development_settings_enabled", "1"},
        {GLOBAL, "wifi_scan_always_enabled", "0"},
        {GLOBAL, "network_recommendations_enabled", "0"},
        {GLOBAL, "upload_apk_enable", "0"},
        {GLOBAL, "package_verifier_user_consent", "-1"},
        {GLOBAL, "send_action_app_error", "0"},
        {SECURE, "location_mode", "0"},
    };

    /**
     * When Wireless debugging was last switched back on. Android switches it off whenever Wi-Fi
     * drops or the TV roams to another access point, and some firmware does so again straight
     * away; without this a watch job could spend a burst of writes losing that argument.
     */
    private static long lastSwitchedOn;
    private static final long MIN_GAP = 30 * 1000L;

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
                if (ADB_WIFI.equals(key)) {
                    long now = SystemClock.elapsedRealtime();
                    if (lastSwitchedOn != 0 && now - lastSwitchedOn < MIN_GAP) {
                        Log.i(TAG, key + ": switched off again at once; leaving it to the next check");
                        continue;
                    }
                    lastSwitchedOn = now;
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

    /** Schedules both jobs. Called from everywhere that runs, because neither is guaranteed. */
    static void schedule(Context context) {
        schedulePeriodic(context);
        scheduleWatch(context);
    }

    /** Makes sure the backstop job exists at the interval we want (it survives reboots). */
    private static void schedulePeriodic(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        if (scheduler == null) {
            return;
        }
        JobInfo pending = scheduler.getPendingJob(JOB_ID);
        if (pending != null && pending.getIntervalMillis() == EVERY) {
            return;
        }
        scheduler.schedule(new JobInfo.Builder(JOB_ID, new ComponentName(context, KeepOnJob.class))
                .setPeriodic(EVERY)
                .setPersisted(true)
                .build());
    }

    /**
     * Watches the Wireless debugging setting itself, so the TV is answering again seconds after
     * Android switches it off instead of up to a quarter of an hour later. A job with a content
     * trigger cannot be periodic or persisted, so it fires once and everything that runs
     * schedules it again. It waits for a network, which is what stops it from arguing with
     * Android while Wi-Fi is really down (Android switches Wireless debugging off for that).
     */
    private static void scheduleWatch(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        if (scheduler == null || scheduler.getPendingJob(JOB_ID_WATCH) != null) {
            return;
        }
        rearmWatch(context);
    }

    /**
     * Arms the next watch, whether or not one looks armed already. A job that is running still
     * counts as pending, so this is what the watch job itself must call: the guarded
     * {@link #scheduleWatch} would find "itself" and arm nothing, and the watch would be used up.
     */
    static void rearmWatch(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        if (scheduler == null) {
            return;
        }
        scheduler.schedule(new JobInfo.Builder(JOB_ID_WATCH, new ComponentName(context, WatchJob.class))
                .addTriggerContentUri(new JobInfo.TriggerContentUri(Settings.Global.getUriFor(ADB_WIFI), 0))
                .setTriggerContentUpdateDelay(10 * 1000L)
                .setTriggerContentMaxDelay(30 * 1000L)
                .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY)
                .build());
    }
}
