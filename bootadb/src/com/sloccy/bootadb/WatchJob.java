package com.sloccy.bootadb;

import android.app.job.JobParameters;
import android.app.job.JobService;

/**
 * Runs within seconds of the Wireless debugging setting changing, so a Wi-Fi blip or a roam to
 * another access point costs seconds of being unreachable rather than up to the backstop job's
 * quarter of an hour. A content trigger fires once, so this arms the next one itself.
 */
public final class WatchJob extends JobService {
    @Override
    public boolean onStartJob(JobParameters params) {
        Adb.ensureAll(this);  // first: a scheduling failure must never cost the settings
        Adb.rearmWatch(this); // unguarded: a running job still counts as pending
        return false; // nothing left running
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        return false;
    }
}
