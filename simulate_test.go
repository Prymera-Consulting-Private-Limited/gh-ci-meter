package main

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func at(minutes float64) time.Time {
	return t0.Add(time.Duration(minutes * float64(time.Minute)))
}

func mins(m float64) time.Duration {
	return time.Duration(m * float64(time.Minute))
}

func TestReplayOneRunnerQueuesJobsInArrivalOrder(t *testing.T) {
	jobs := []simJob{
		{Arrived: at(0), Duration: mins(8)},
		{Arrived: at(1), Duration: mins(1)},
		{Arrived: at(2), Duration: mins(1)},
	}

	got := replay(jobs, 1)

	// The second job waits for the first to finish at 8, the third for the
	// second to finish at 9.
	want := []time.Duration{0, mins(7), mins(7)}
	if !slices.Equal(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestReplayGivesEachJobTheRunnerThatFreesFirst(t *testing.T) {
	jobs := []simJob{
		{Arrived: at(0), Duration: mins(8)},
		{Arrived: at(0), Duration: mins(2)},
		{Arrived: at(1), Duration: mins(1)},
	}

	got := replay(jobs, 2)

	// The third job takes the runner that finished the two-minute job.
	want := []time.Duration{0, 0, mins(1)}
	if !slices.Equal(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestReplaySortsJobsThatArriveOutOfOrder(t *testing.T) {
	jobs := []simJob{
		{Arrived: at(1), Duration: mins(1)},
		{Arrived: at(0), Duration: mins(5)},
	}

	got := replay(jobs, 1)

	want := []time.Duration{0, mins(4)}
	if !slices.Equal(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestSummariseWaits(t *testing.T) {
	waits := []time.Duration{mins(10), 0, mins(1), mins(2), mins(6), 0, 0, 0, 0, 0}

	got := summariseWaits(waits)

	if time.Duration(got.Median) != 0 {
		t.Errorf("median = %v, want 0", time.Duration(got.Median))
	}
	if time.Duration(got.P90) != mins(6) {
		t.Errorf("p90 = %v, want 6m", time.Duration(got.P90))
	}
	if time.Duration(got.Max) != mins(10) {
		t.Errorf("max = %v, want 10m", time.Duration(got.Max))
	}
	if got.OverLimit != 2 {
		t.Errorf("over 5 minutes = %d, want 2", got.OverLimit)
	}
}

func TestPercentileOfNothingIsZero(t *testing.T) {
	if got := percentile(nil, 0.9); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}
