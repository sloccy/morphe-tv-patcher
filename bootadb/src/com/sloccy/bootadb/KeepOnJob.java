package com.sloccy.bootadb;

import android.app.job.JobParameters;
import android.app.job.JobService;

/** The backstop, every 15 minutes: switch Wireless debugging back on if the TV turned it off. */
public final class KeepOnJob extends JobService {
    @Override
    public boolean onStartJob(JobParameters params) {
        Adb.ensureAll(this); // first: a scheduling failure must never cost the settings
        Adb.schedule(this);  // the watch job is not persisted, so keep it alive from here too
        return false; // nothing left running
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        return false;
    }
}
