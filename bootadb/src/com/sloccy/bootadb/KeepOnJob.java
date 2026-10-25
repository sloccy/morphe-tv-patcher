package com.sloccy.bootadb;

import android.app.job.JobParameters;
import android.app.job.JobService;

/** Every hour: switch Wireless debugging back on if the TV turned it off. */
public final class KeepOnJob extends JobService {
    @Override
    public boolean onStartJob(JobParameters params) {
        Adb.ensureAll(this);
        return false; // nothing left running
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        return false;
    }
}
