package com.sloccy.bootadb;

import android.app.job.JobParameters;
import android.app.job.JobService;

/**
 * Runs a little after the helper switched Wireless debugging on, and does something only if
 * Android switched it off again (an access point the TV has not been told to trust, or Wi-Fi not
 * ready yet). Then it switches it on once more and waits longer before the next look.
 */
public final class RetryJob extends JobService {
    @Override
    public boolean onStartJob(JobParameters params) {
        int step = params.getExtras().getInt(Adb.EXTRA_STEP, 1);
        Adb.ensureAll(this, step); // writes and schedules the next retry only if it is off
        Adb.schedule(this);
        return false; // nothing left running
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        return false;
    }
}
