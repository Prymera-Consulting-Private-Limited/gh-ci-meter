package main

import (
	"math"
	"testing"
	"time"
)

// ranJob is a job a runner picked up after waiting, then ran for a while.
func ranJob(workflow, name string, queued, wait, run time.Duration, hosted bool) workflowJob {
	job := workflowJob{
		Name:         name,
		WorkflowName: workflow,
		Status:       "completed",
		Conclusion:   "success",
		CreatedAt:    t0.Add(queued),
		StartedAt:    t0.Add(queued + wait),
		CompletedAt:  t0.Add(queued + wait + run),
	}
	if hosted {
		job.Labels = []string{"ubuntu-latest"}
		job.RunnerName = "GitHub Actions 7"
		job.RunnerGroupName = hostedRunnerGroup
	} else {
		job.Labels = []string{"self-hosted", "linux"}
		job.RunnerName = "build-box"
		job.RunnerGroupName = "Default"
	}
	return job
}

func oneRun(jobs ...workflowJob) runWithJobs {
	return runWithJobs{Run: workflowRun{Name: "Tests", Status: "completed", Conclusion: "success"}, Jobs: jobs}
}

func TestCostRoundsEachJobUpToAWholeMinute(t *testing.T) {
	runs := []runWithJobs{oneRun(
		ranJob("Tests", "unit", 0, 0, mins(8)+time.Second, true),
		ranJob("Tests", "lint", 0, 0, 20*time.Second, true),
		ranJob("Tests", "suite", 0, 0, mins(10), false),
	)}

	cost := costOf(jobRecords(runs), nil, 30*24*time.Hour, false)

	if cost.HostedMinutes != 10 {
		t.Errorf("hosted minutes = %d, want 10 (9 + 1)", cost.HostedMinutes)
	}
	if math.Abs(cost.HostedUSD-0.06) > 1e-9 {
		t.Errorf("hosted cost = %v, want 0.06 at $0.006/min", cost.HostedUSD)
	}
	if cost.SelfHostedMinutes != 10 {
		t.Errorf("self-hosted minutes = %d, want 10", cost.SelfHostedMinutes)
	}
	if math.Abs(cost.SelfHostedAtFeeUSD-0.02) > 1e-9 {
		t.Errorf("self-hosted at the proposed fee = %v, want 0.02", cost.SelfHostedAtFeeUSD)
	}
}

func TestCostLeavesUnknownLabelsOutAndSaysWhich(t *testing.T) {
	job := ranJob("Tests", "big", 0, 0, mins(3), true)
	job.Labels = []string{"linux-16-core"}

	cost := costOf(jobRecords([]runWithJobs{oneRun(job)}), nil, 24*time.Hour, false)

	if cost.HostedUSD != 0 || cost.UnpricedMinutes != 3 {
		t.Errorf("cost = %v, unpriced = %d; want 0 and 3", cost.HostedUSD, cost.UnpricedMinutes)
	}
	if len(cost.UnpricedLabels) != 1 || cost.UnpricedLabels[0] != "linux-16-core" {
		t.Errorf("unpriced labels = %v", cost.UnpricedLabels)
	}

	priced := costOf(jobRecords([]runWithJobs{oneRun(job)}), map[string]float64{"linux-16-core": 0.042}, 24*time.Hour, false)
	if math.Abs(priced.HostedUSD-0.126) > 1e-9 {
		t.Errorf("cost with --rate = %v, want 0.126", priced.HostedUSD)
	}
}

func TestStandardRunnersAreFreeInAPublicRepositoryButLargerOnesAreNot(t *testing.T) {
	standard := ranJob("Tests", "unit", 0, 0, mins(10), true)
	larger := ranJob("Tests", "big", 0, 0, mins(10), true)
	larger.Labels = []string{"macos-15-xlarge"}

	cost := costOf(jobRecords([]runWithJobs{oneRun(standard, larger)}), nil, 24*time.Hour, true)

	if cost.FreeMinutes != 10 || cost.ChargedMinutes != 10 {
		t.Errorf("free %d, charged %d; want 10 and 10", cost.FreeMinutes, cost.ChargedMinutes)
	}
	if math.Abs(cost.HostedUSD-1.02) > 1e-9 {
		t.Errorf("cost = %v, want 1.02 for the larger runner alone", cost.HostedUSD)
	}
}

func TestDependabotMinutesAreFreeInAPrivateRepository(t *testing.T) {
	dependabot := runWithJobs{
		Run:  workflowRun{Name: "npm in / - Update #1", Event: "dynamic", Path: "dynamic/dependabot/dependabot-updates", Conclusion: "success"},
		Jobs: []workflowJob{ranJob("npm in / - Update #1", "Dependabot", 0, 0, mins(4), true)},
	}
	tests := oneRun(ranJob("Tests", "unit", 0, 0, mins(5), true))

	cost := costOf(jobRecords([]runWithJobs{dependabot, tests}), nil, 24*time.Hour, false)

	if cost.FreeMinutes != 4 || cost.ChargedMinutes != 5 {
		t.Errorf("free %d, charged %d; want 4 and 5", cost.FreeMinutes, cost.ChargedMinutes)
	}
	if math.Abs(cost.HostedUSD-0.03) > 1e-9 {
		t.Errorf("cost = %v, want 0.03 for the tests alone", cost.HostedUSD)
	}
}

func TestJobsThatNeverReachedARunnerAreNotCounted(t *testing.T) {
	skipped := workflowJob{
		Name: "deploy", WorkflowName: "Tests", Conclusion: "skipped",
		CreatedAt: t0, StartedAt: t0.Add(time.Second), CompletedAt: t0,
	}
	queuedThenCancelled := workflowJob{
		Name: "suite", WorkflowName: "Tests", Conclusion: "cancelled",
		CreatedAt: t0, StartedAt: t0, CompletedAt: t0.Add(mins(3)),
	}

	records := jobRecords([]runWithJobs{oneRun(skipped, queuedThenCancelled)})

	if stats := statsByJob(records); len(stats) != 0 {
		t.Errorf("stats = %v, want none", stats)
	}
	if cost := costOf(records, nil, 24*time.Hour, false); cost.HostedMinutes+cost.SelfHostedMinutes != 0 {
		t.Errorf("billed minutes for jobs that never ran: %+v", cost)
	}
}

func TestLaneCandidateIsQuickButOneTimeInTenWaitsLongerThanItRuns(t *testing.T) {
	stats := []jobStats{
		// A short median wait can hide a bad queue: this window held days on
		// GitHub's runners (no wait) and days behind one shared runner.
		{Workflow: "Pint", Job: "Pint", MedianRun: duration(24 * time.Second), P90Run: duration(28 * time.Second),
			MedianWait: duration(3 * time.Second), P90Wait: duration(mins(17))},
		{Workflow: "Tests", Job: "PHPUnit", MedianRun: duration(mins(8)), P90Run: duration(mins(9)),
			MedianWait: duration(mins(3)), P90Wait: duration(mins(9))},
		{Workflow: "Docs", Job: "links", MedianRun: duration(30 * time.Second), P90Run: duration(40 * time.Second),
			MedianWait: duration(5 * time.Second), P90Wait: duration(8 * time.Second)},
	}

	lanes := laneCandidates(stats)

	if len(lanes) != 1 || lanes[0].Job != "Pint" {
		t.Errorf("lanes = %v, want only Pint", lanes)
	}
}

func TestSelfHostedUseCountsEachRunnerFromItsFirstJob(t *testing.T) {
	early := ranJob("Tests", "suite", 0, 0, mins(30), false)
	late := ranJob("Tests", "suite", mins(90), 0, mins(30), false)
	late.RunnerName = "second-box"

	// The first runner is counted for two hours, the second for half an hour:
	// an hour of work over two and a half hours of runner time.
	use := selfHostedUseOf(jobRecords([]runWithJobs{oneRun(early, late)}), t0.Add(mins(120)))

	if use == nil || use.Runners != 2 {
		t.Fatalf("use = %+v, want two runners", use)
	}
	if math.Abs(use.BusyShare-0.4) > 1e-9 {
		t.Errorf("busy share = %v, want 0.4", use.BusyShare)
	}
}

func TestDynamicRunsAreGroupedByTheirPath(t *testing.T) {
	update := func(name string) runWithJobs {
		job := ranJob(name, "Dependabot", 0, 0, mins(2), true)
		return runWithJobs{
			Run:  workflowRun{Name: name, Event: "dynamic", Path: "dynamic/dependabot/dependabot-updates", Conclusion: "success"},
			Jobs: []workflowJob{job},
		}
	}

	stats := statsByJob(jobRecords([]runWithJobs{update("npm in / for vue - Update #1"), update("composer in / - Update #2")}))

	if len(stats) != 1 || stats[0].Workflow != "dynamic/dependabot/dependabot-updates" || stats[0].Count != 2 {
		t.Errorf("stats = %+v, want one group of two under the dynamic path", stats)
	}
}

func TestRunnerCountFindsTheSmallestPoolThatMeetsTheTarget(t *testing.T) {
	// Three eight-minute jobs queued together: one runner makes the last wait 16
	// minutes, two make it wait 8, three let them all start at once.
	runs := []runWithJobs{oneRun(
		ranJob("Tests", "a", 0, 0, mins(8), false),
		ranJob("Tests", "b", 0, mins(8), mins(8), false),
		ranJob("Tests", "c", 0, mins(16), mins(8), false),
	)}

	count := runnerCountFor(jobRecords(runs), nil, analysisOptions{TargetWait: time.Minute, MaxRunners: 10})

	if count.Suggested != 3 {
		t.Errorf("suggested = %d, want 3", count.Suggested)
	}
	if len(count.Scenarios) != 3 {
		t.Errorf("scenarios = %d, want 3 (it stops at the first that meets the target)", len(count.Scenarios))
	}
	if got := time.Duration(count.Scenarios[0].Shared.Max); got != mins(16) {
		t.Errorf("longest wait on one runner = %v, want 16m", got)
	}
}

func TestRunnerCountWithALaneTakesTheQuickJobsOutOfTheQueue(t *testing.T) {
	runs := []runWithJobs{oneRun(
		ranJob("Tests", "suite", 0, 0, mins(8), false),
		ranJob("Pint", "Pint", 0, mins(8), 20*time.Second, false),
	)}
	records := jobRecords(runs)
	lanes := []jobStats{{Workflow: "Pint", Job: "Pint"}}

	count := runnerCountFor(records, lanes, analysisOptions{TargetWait: time.Minute, MaxRunners: 10})

	first := count.Scenarios[0]
	if time.Duration(first.Shared.Max) != mins(8) {
		t.Errorf("shared longest wait = %v, want 8m", time.Duration(first.Shared.Max))
	}
	if first.WithLane == nil || time.Duration(first.WithLane.Max) != 0 {
		t.Errorf("with a lane = %+v, want no wait", first.WithLane)
	}
	if count.SuggestedWithLane != 1 {
		t.Errorf("suggested with a lane = %d, want 1", count.SuggestedWithLane)
	}
}

func TestCancelledBeforeStartCountsOnlyRunsNoRunnerTouched(t *testing.T) {
	waitingRunCancelled := runWithJobs{Run: workflowRun{Name: "Tests", Event: "push", HeadBranch: "develop", Conclusion: "cancelled"}}
	cancelledMidRun := runWithJobs{
		Run:  workflowRun{Name: "Tests", Event: "push", HeadBranch: "develop", Conclusion: "cancelled"},
		Jobs: []workflowJob{ranJob("Tests", "suite", 0, 0, mins(2), false)},
	}
	pullRequestCancelled := runWithJobs{Run: workflowRun{Name: "Tests", Event: "pull_request", HeadBranch: "feature", Conclusion: "cancelled"}}

	got := cancelledBeforeStart([]runWithJobs{waitingRunCancelled, cancelledMidRun, pullRequestCancelled}, "develop")

	if len(got) != 2 {
		t.Fatalf("groups = %v, want 2", got)
	}
	for _, c := range got {
		if c.Runs != 1 {
			t.Errorf("%+v: runs = %d, want 1", c, c.Runs)
		}
		if c.Event == "push" && !c.DefaultBranch {
			t.Errorf("push run on develop not marked as the default branch: %+v", c)
		}
	}
}

func TestBusiestHoursSplitsAJobAcrossTheHoursItRanIn(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no time zone database")
	}
	// 12:00 UTC is 17:30 in Kolkata. A job from 12:20 to 12:50 UTC runs from
	// 17:50 to 18:20 local: ten minutes in the 17:00 hour, twenty in the 18:00.
	runs := []runWithJobs{oneRun(ranJob("Tests", "suite", mins(20), 0, mins(30), false))}

	hours := busiestHours(jobRecords(runs), kolkata, 5)

	if len(hours) != 2 {
		t.Fatalf("hours = %v, want 2", hours)
	}
	if hours[0].Start.Hour() != 18 || math.Round(hours[0].BusyMinutes) != 20 {
		t.Errorf("busiest = %s with %.0f min, want 18:00 with 20", hours[0].Start.Format("15:04"), hours[0].BusyMinutes)
	}
	if hours[1].Start.Hour() != 17 || math.Round(hours[1].BusyMinutes) != 10 {
		t.Errorf("second = %s with %.0f min, want 17:00 with 10", hours[1].Start.Format("15:04"), hours[1].BusyMinutes)
	}
	if hours[1].JobsStarted != 1 || hours[0].JobsStarted != 0 {
		t.Errorf("jobs started = %d and %d, want the start counted in 17:00 only", hours[1].JobsStarted, hours[0].JobsStarted)
	}
}
