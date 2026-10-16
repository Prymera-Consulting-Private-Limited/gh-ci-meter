package main

import (
	"math"
	"slices"
	"sort"
	"time"
)

// simJob is a job as the replay needs it: when it asked for a runner and how
// long it held one.
type simJob struct {
	Arrived  time.Time
	Duration time.Duration
}

// replay runs the jobs, in the order they asked for a runner, on a pool of
// identical runners that each take one job at a time, and returns how long each
// job would have waited. A job goes to whichever runner frees up first.
func replay(jobs []simJob, runners int) []time.Duration {
	ordered := slices.Clone(jobs)
	sort.SliceStable(ordered, func(a, b int) bool {
		return ordered[a].Arrived.Before(ordered[b].Arrived)
	})

	freeAt := make([]time.Time, runners)
	waits := make([]time.Duration, len(ordered))
	for i, job := range ordered {
		soonest := 0
		for r := range freeAt {
			if freeAt[r].Before(freeAt[soonest]) {
				soonest = r
			}
		}

		start := job.Arrived
		if freeAt[soonest].After(start) {
			start = freeAt[soonest]
		}
		waits[i] = start.Sub(job.Arrived)
		freeAt[soonest] = start.Add(job.Duration)
	}
	return waits
}

type waitSummary struct {
	Median    duration `json:"median_wait_seconds"`
	P90       duration `json:"p90_wait_seconds"`
	Max       duration `json:"max_wait_seconds"`
	OverLimit int      `json:"jobs_waiting_over_5_minutes"`
}

// A wait past this is one a developer notices and goes to look at.
const noticeableWait = 5 * time.Minute

func summariseWaits(waits []time.Duration) waitSummary {
	sorted := slices.Clone(waits)
	slices.Sort(sorted)

	over := 0
	for _, w := range sorted {
		if w > noticeableWait {
			over++
		}
	}
	return waitSummary{
		Median:    duration(percentile(sorted, 0.5)),
		P90:       duration(percentile(sorted, 0.9)),
		Max:       duration(percentile(sorted, 1)),
		OverLimit: over,
	}
}

// percentile reads the nearest-rank percentile from an ascending slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}
