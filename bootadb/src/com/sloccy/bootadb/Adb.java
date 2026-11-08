package com.sloccy.bootadb;

import android.app.job.JobInfo;
import android.app.job.JobScheduler;
import android.content.ComponentName;
import android.content.ContentResolver;
import android.content.Context;
import android.os.PersistableBundle;
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
    private static final int JOB_ID_RETRY = 3;

    /** Extra on the retry job: which wait in {@link #RETRY_WAITS} it is on. */
    static final String EXTRA_STEP = "step";

    /**
     * How long to wait before checking that Wireless debugging stayed on after we switched it on.
     * Android switches it straight off again when the TV is on an access point it has not been
     * told to trust ("Always allow on this network" is per BSSID, and every access point and band
     * has its own) or Wi-Fi is not ready yet. The last wait repeats until it sticks.
     */
    private static final long[] RETRY_WAITS = {
        30 * 1000L, 60 * 1000L, 2 * 60 * 1000L, 4 * 60 * 1000L, 8 * 60 * 1000L, 15 * 60 * 1000L,
    };

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
        // Legacy ADB over TCP (TV_FIXED_ADB_PORT) needs ADB debugging itself on.
        {GLOBAL, "adb_enabled", "1"},
        // Developer options off takes Wireless debugging with it.
        {GLOBAL, "development_settings_enabled", "1"},
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
        ensureAll(context, -1);
    }

    /**
     * As {@link #ensureAll(Context)}. {@code retryStep} is the retry job's own step (0 or more), or
     * -1 from anywhere else. Anywhere else leaves Wireless debugging alone while a retry is
     * pending: Android's switch-off after our write fires the watch job, and answering that at
     * once would only start the argument again at the shortest wait.
     */
    static void ensureAll(Context context, int retryStep) {
        ContentResolver resolver = context.getContentResolver();
        int changed = 0;
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
                    if (retryStep < 0 && retryPending(context)) {
                        Log.i(TAG, key + ": off; a retry is already scheduled");
                        continue;
                    }
                    if (retryStep > 0) {
                        Log.w(TAG, key + ": Android switched it off again after step " + (retryStep - 1)
                                + " (this access point is probably not trusted for Wireless debugging, or Wi-Fi was not ready)");
                    }
                }
                boolean ok = global ? Settings.Global.putString(resolver, key, want) : Settings.Secure.putString(resolver, key, want);
                changed++;
                Log.i(TAG, key + ": " + have + " -> " + want + (ok ? "" : " (not accepted)"));
                if (ADB_WIFI.equals(key)) {
                    scheduleRetry(context, Math.max(retryStep, 0));
                }
            } catch (SecurityException e) {
                Log.w(TAG, key + ": WRITE_SECURE_SETTINGS is not granted");
            } catch (RuntimeException e) {
                Log.w(TAG, key + ": " + e);
            }
        }
        // Always say something: an empty log cannot be told apart from never having run, which is
        // exactly the ambiguity that hid this app doing nothing at all for weeks.
        Log.i(TAG, "checked " + WANTED.length + " settings, changed " + changed);
    }

    /**
     * Schedules both jobs. Called from everywhere that runs, because neither is guaranteed. Each
     * one is on its own: a JobInfo the system will not accept must not stop the other, and must
     * never stop the settings, which is why every caller applies those first.
     */
    static void schedule(Context context) {
        try {
            schedulePeriodic(context);
        } catch (RuntimeException e) {
            Log.w(TAG, "could not schedule the 15-minute check: " + e);
        }
        try {
            scheduleWatch(context);
        } catch (RuntimeException e) {
            Log.w(TAG, "could not schedule the watch: " + e);
        }
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

    private static boolean retryPending(Context context) {
        JobScheduler scheduler = context.getSystemService(JobScheduler.class);
        return scheduler != null && scheduler.getPendingJob(JOB_ID_RETRY) != null;
    }

    /**
     * Checks again after RETRY_WAITS[step] that Wireless debugging is still on, and switches it on
     * again if Android undid it. Replaces any retry already scheduled.
     */
    private static void scheduleRetry(Context context, int step) {
        try {
            JobScheduler scheduler = context.getSystemService(JobScheduler.class);
            if (scheduler == null) {
                return;
            }
            PersistableBundle extras = new PersistableBundle();
            extras.putInt(EXTRA_STEP, step + 1); // the step the retry will run as
            long wait = RETRY_WAITS[Math.min(step, RETRY_WAITS.length - 1)];
            scheduler.schedule(new JobInfo.Builder(JOB_ID_RETRY, new ComponentName(context, RetryJob.class))
                    .setMinimumLatency(wait)
                    .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY)
                    .setExtras(extras)
                    .build());
        } catch (RuntimeException e) {
            Log.w(TAG, "could not schedule the retry: " + e);
        }
    }
}
