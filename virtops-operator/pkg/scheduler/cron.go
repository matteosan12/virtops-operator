package scheduler

import (
	"time"

	"github.com/robfig/cron/v3"
)

// NextFromCron computes the next occurrence starting from a reference time.
// It supports the standard 5-field cron syntax (min, hour, day, month, weekday).
func NextFromCron(spec string, from time.Time) (time.Time, error) {
	sch, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, err
	}
	return sch.Next(from), nil
}

// IsDue determines whether, given a schedule and the last known run, the policy is due now.
// It also returns the next scheduled run time.
func IsDue(spec string, lastRun *time.Time, now time.Time) (bool, time.Time, error) {
	if spec == "" {
		return false, time.Time{}, nil
	}
	sch, err := cron.ParseStandard(spec)
	if err != nil {
		return false, time.Time{}, err
	}
	if lastRun == nil || lastRun.IsZero() {
		return true, sch.Next(now), nil
	}
	nextAfterLast := sch.Next(lastRun.Add(time.Nanosecond))
	due := !now.Before(nextAfterLast)
	return due, sch.Next(now), nil
}
