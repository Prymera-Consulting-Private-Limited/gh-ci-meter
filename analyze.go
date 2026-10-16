package main

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// A lane candidate is a quick job that, replayed on one runner fewer than a
// shared pool needs, would wait longer than it runs one time in ten: it would be
// queuing behind longer jobs, and a runner of its own would let the pool shrink.
//
// The replay decides it, not the waits the job actually had. On GitHub's runners
// a job never queues behind your other jobs, so its own waits cannot show what
// would happen on runners of yours, and a window spanning a move to your own
// runners mixes the two. The slowest tenth is used rather than the median
// because that is when a merge is held up.
const (
	laneMaxRun  = 2 * time.Minute
	laneMinWait = time.Minute
)

// duration marshals as whole seconds, which is what a reader of the JSON wants.
type duration time.Duration

func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d).Round(time.Second) / time.Second))
}

type Report struct {
	Repo          string          `json:"repo"`
	Private       bool            `json:"private"`
	DefaultBranch string          `json:"default_branch"`
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	TimeZone      string          `json:"time_zone"`
	Runs          int             `json:"runs"`
	Jobs          int             `json:"jobs"`
	Cost          costSummary     `json:"cost"`
	JobStats      []jobStats      `json:"jobs_by_name"`
	BusiestHours  []hourStats     `json:"busiest_hours"`
	SelfHosted    *selfHostedUse  `json:"self_hosted_use,omitempty"`
	RunnerCount   runnerCount     `json:"runner_count"`
	Lanes         []laneCandidate `json:"lane_candidates"`
	Cancelled     []cancelledRuns `json:"cancelled_before_start"`
	Warnings      []string        `json:"warnings"`

	location *time.Location
}

// zoneName is how the report names its time zone. The local zone is called
// "Local" by Go, which tells a reader of a shared report nothing.
func zoneName(loc *time.Location) string {
	if loc.String() != "Local" {
		return loc.String()
	}
	return time.Now().In(loc).Format("MST (UTC-07:00)")
}

// costSummary splits GitHub-hosted minutes three ways: charged, free (a public
// repository, Dependabot, GitHub Pages) and unpriced (a label with no known rate).
// Charged minutes are split again by where each job ran last. A job whose latest
// run was on your own runners has already moved, so its cost is a saving made,
// not one still to make.
type costSummary struct {
	Days                 float64    `json:"days"`
	Hosted               []costLine `json:"hosted"`
	HostedMinutes        int        `json:"hosted_minutes"`
	ChargedMinutes       int        `json:"charged_minutes"`
	HostedUSD            float64    `json:"hosted_usd"`
	StillHostedMinutes   int        `json:"still_hosted_charged_minutes"`
	StillHostedUSD       float64    `json:"still_hosted_usd"`
	StillHostedAtFeeUSD  float64    `json:"still_hosted_minutes_self_hosted_at_proposed_fee_usd"`
	MovedJobs            []string   `json:"moved_to_self_hosted_jobs"`
	MovedUSD             float64    `json:"moved_to_self_hosted_usd"`
	FreeMinutes          int        `json:"free_minutes"`
	UnpricedLabels       []string   `json:"unpriced_hosted_labels"`
	UnpricedMinutes      int        `json:"unpriced_hosted_minutes"`
	SelfHostedMinutes    int        `json:"self_hosted_minutes"`
	SelfHostedAtFeeUSD   float64    `json:"self_hosted_usd_at_proposed_fee"`
	RatesCheckedOn       string     `json:"rates_checked_on"`
	RatesSource          string     `json:"rates_source"`
	FreeUsageSource      string     `json:"free_usage_source"`
	ProposedFeePerMinute float64    `json:"proposed_self_hosted_fee_per_minute"`
	ProposedFeeSource    string     `json:"proposed_self_hosted_fee_source"`
}

type costLine struct {
	Runner       string  `json:"runner"`
	USDPerMinute float64 `json:"usd_per_minute"`
	FreeBecause  string  `json:"free_because,omitempty"`
	Jobs         int     `json:"jobs"`
	Minutes      int     `json:"minutes"`
	USD          float64 `json:"usd"`
}

type jobStats struct {
	Workflow   string   `json:"workflow"`
	Job        string   `json:"job"`
	RunsOn     string   `json:"runs_on"`
	Count      int      `json:"jobs"`
	Minutes    int      `json:"minutes"`
	MedianRun  duration `json:"median_run_seconds"`
	P90Run     duration `json:"p90_run_seconds"`
	MedianWait duration `json:"median_wait_seconds"`
	P90Wait    duration `json:"p90_wait_seconds"`
	MaxWait    duration `json:"max_wait_seconds"`
}

func (s jobStats) key() string {
	return jobKey(s.Workflow, s.Job)
}

type hourStats struct {
	Start       time.Time `json:"start"`
	BusyMinutes float64   `json:"busy_runner_minutes"`
	JobsStarted int       `json:"jobs_started"`
	MaxWait     duration  `json:"max_wait_seconds"`
}

// selfHostedUse counts runners rather than naming them: runner names are often
// machine names, and the report is meant to be shareable.
type selfHostedUse struct {
	Runners   int     `json:"runners"`
	BusyShare float64 `json:"busy_share"`
}

type runnerCount struct {
	TargetP90         duration   `json:"target_p90_wait_seconds"`
	Suggested         int        `json:"suggested_runners"`
	SuggestedWithLane int        `json:"suggested_runners_with_lane"`
	LaneCheckRunners  int        `json:"lane_check_runners"`
	LaneJobs          []string   `json:"lane_jobs"`
	Scenarios         []scenario `json:"scenarios"`
}

// laneCandidate is a quick job that would queue behind longer ones on runners
// of your own: one time in ten it waits ReplayP90Wait in the replay on
// ReplayRunners runners. Its own waits, as it really ran, are in jobStats.
type laneCandidate struct {
	jobStats
	ReplayRunners int      `json:"replayed_on_runners"`
	ReplayP90Wait duration `json:"replayed_p90_wait_seconds"`
}

// scenario is one pool size. WithLane gives the lane jobs one runner of their
// own on top of Runners, which take everything else.
type scenario struct {
	Runners  int          `json:"runners"`
	Shared   waitSummary  `json:"shared"`
	WithLane *waitSummary `json:"with_lane,omitempty"`
}

type cancelledRuns struct {
	Workflow      string `json:"workflow"`
	Event         string `json:"event"`
	DefaultBranch bool   `json:"on_default_branch"`
	Runs          int    `json:"runs"`
}

// jobRecord is one job as the analysis sees it.
type jobRecord struct {
	Workflow    string
	Name        string
	FreeBecause string // the GitHub service whose standard-runner minutes are free, if any
	// StartedByGitHub marks Dependabot, GitHub Pages and the other runs GitHub
	// starts itself. Changing runs-on in your workflows does not move them.
	StartedByGitHub bool
	Hosted          bool
	Labels          []string
	Runner          string
	Queued          time.Time
	Started         time.Time
	Finished        time.Time
}

// ran says whether a runner picked the job up and finished it. A skipped job,
// or one cancelled while it was queued, never had a runner.
func (j jobRecord) ran() bool {
	return j.Runner != "" && !j.Started.IsZero() && !j.Finished.IsZero() && !j.Finished.Before(j.Started)
}

func (j jobRecord) wait() time.Duration {
	return max(j.Started.Sub(j.Queued), 0)
}

func (j jobRecord) duration() time.Duration {
	return j.Finished.Sub(j.Started)
}

// billedMinutes is the job's time as GitHub bills it: rounded up to a whole minute.
func (j jobRecord) billedMinutes() int {
	return int(math.Ceil(j.duration().Minutes()))
}

func (j jobRecord) key() string {
	return jobKey(j.Workflow, j.Name)
}

func jobKey(workflow, job string) string {
	return fmt.Sprintf("%s / %s", workflow, job)
}

type analysisOptions struct {
	Location   *time.Location
	TargetWait time.Duration
	MaxRunners int
	Rates      map[string]float64
}

func analyze(info repoInfo, runs []runWithJobs, from, to time.Time, opts analysisOptions) Report {
	records := jobRecords(runs)

	report := Report{
		Repo:          info.FullName,
		Private:       info.Private,
		DefaultBranch: info.DefaultBranch,
		From:          from,
		To:            to,
		TimeZone:      zoneName(opts.Location),
		location:      opts.Location,
		Runs:          len(runs),
		Jobs:          len(records),
		Cost:          costOf(records, opts.Rates, to.Sub(from), !info.Private),
		JobStats:      statsByJob(records),
		BusiestHours:  busiestHours(records, opts.Location, 5),
		SelfHosted:    selfHostedUseOf(records, to),
		Cancelled:     cancelledBeforeStart(runs, info.DefaultBranch),
	}
	report.RunnerCount, report.Lanes = runnerCountFor(records, report.JobStats, opts)
	report.Warnings = warningsFor(report)
	return report
}

func jobRecords(runs []runWithJobs) []jobRecord {
	var records []jobRecord
	for _, r := range runs {
		for _, job := range r.Jobs {
			workflow := job.WorkflowName
			// Dependabot and other dynamic runs are named after what they do on
			// the day, so each would look like a workflow of its own. Their
			// path is the same every time.
			if r.Run.Event == "dynamic" && r.Run.Path != "" {
				workflow = r.Run.Path
			}
			if workflow == "" {
				workflow = r.Run.Name
			}
			records = append(records, jobRecord{
				Workflow:        workflow,
				Name:            job.Name,
				FreeBecause:     freeService(r.Run),
				StartedByGitHub: r.Run.Event == "dynamic",
				Hosted:          isHosted(job),
				Labels:          job.Labels,
				Runner:          job.RunnerName,
				Queued:          job.CreatedAt,
				Started:         job.StartedAt,
				Finished:        job.CompletedAt,
			})
		}
	}
	return records
}

// freeService names the GitHub service a run belongs to when GitHub does not
// charge for its standard-runner minutes, wherever the repository is.
func freeService(run workflowRun) string {
	if run.Event != "dynamic" {
		return ""
	}
	switch {
	case strings.HasPrefix(run.Path, "dynamic/dependabot/"):
		return "Dependabot"
	case strings.HasPrefix(run.Path, "dynamic/pages/"):
		return "GitHub Pages"
	}
	return ""
}

func costOf(records []jobRecord, overrides map[string]float64, window time.Duration, public bool) costSummary {
	summary := costSummary{
		Days:                 window.Hours() / 24,
		RatesCheckedOn:       ratesCheckedOn,
		RatesSource:          ratesSource,
		FreeUsageSource:      freeUsageSource,
		ProposedFeePerMinute: proposedSelfHostedFee,
		ProposedFeeSource:    proposedFeeSource,
	}

	stillHosted := lastRanHosted(records)
	lines := map[string]*costLine{}
	unpriced := map[string]bool{}
	moved := map[string]bool{}
	for _, job := range records {
		if !job.ran() {
			continue
		}
		minutes := job.billedMinutes()
		if !job.Hosted {
			summary.SelfHostedMinutes += minutes
			continue
		}

		summary.HostedMinutes += minutes
		r, ok := hostedRate(job.Labels, overrides)
		if !ok {
			summary.UnpricedMinutes += minutes
			unpriced[strings.Join(job.Labels, ", ")] = true
			continue
		}
		freeBecause := ""
		if !r.Larger {
			freeBecause = job.FreeBecause
			if public {
				freeBecause = "public repository"
			}
		}

		// Free minutes get a line of their own at no cost, so the report still
		// shows how much ran on GitHub's runners.
		key := fmt.Sprintf("%s at %v, free: %s", r.Runner, r.USDPerMinute, freeBecause)
		line, seen := lines[key]
		if !seen {
			line = &costLine{Runner: r.Runner, USDPerMinute: r.USDPerMinute, FreeBecause: freeBecause}
			lines[key] = line
		}
		line.Jobs++
		line.Minutes += minutes
		if freeBecause != "" {
			summary.FreeMinutes += minutes
			continue
		}
		summary.ChargedMinutes += minutes
		usd := float64(minutes) * r.USDPerMinute
		line.USD += usd
		if stillHosted[job.key()] {
			summary.StillHostedMinutes += minutes
			summary.StillHostedUSD += usd
		} else {
			summary.MovedUSD += usd
			moved[job.key()] = true
		}
	}

	for _, line := range lines {
		summary.Hosted = append(summary.Hosted, *line)
		summary.HostedUSD += line.USD
	}
	sort.Slice(summary.Hosted, func(a, b int) bool {
		if summary.Hosted[a].USD != summary.Hosted[b].USD {
			return summary.Hosted[a].USD > summary.Hosted[b].USD
		}
		return summary.Hosted[a].Minutes > summary.Hosted[b].Minutes
	})
	for labels := range unpriced {
		summary.UnpricedLabels = append(summary.UnpricedLabels, labels)
	}
	slices.Sort(summary.UnpricedLabels)
	for key := range moved {
		summary.MovedJobs = append(summary.MovedJobs, key)
	}
	slices.Sort(summary.MovedJobs)

	summary.SelfHostedAtFeeUSD = float64(summary.SelfHostedMinutes) * proposedSelfHostedFee
	summary.StillHostedAtFeeUSD = float64(summary.StillHostedMinutes) * proposedSelfHostedFee
	return summary
}

// lastRanHosted says, for each job, whether its most recent run in the window
// was on GitHub's runners.
func lastRanHosted(records []jobRecord) map[string]bool {
	latest := map[string]time.Time{}
	hosted := map[string]bool{}
	for _, job := range records {
		if !job.ran() {
			continue
		}
		if last, seen := latest[job.key()]; !seen || job.Started.After(last) {
			latest[job.key()] = job.Started
			hosted[job.key()] = job.Hosted
		}
	}
	return hosted
}

func statsByJob(records []jobRecord) []jobStats {
	type group struct {
		workflow, job      string
		hosted, selfHosted bool
		runs, waits        []time.Duration
		minutes            int
	}
	groups := map[string]*group{}
	for _, job := range records {
		if !job.ran() {
			continue
		}
		g, seen := groups[job.key()]
		if !seen {
			g = &group{workflow: job.Workflow, job: job.Name}
			groups[job.key()] = g
		}
		if job.Hosted {
			g.hosted = true
		} else {
			g.selfHosted = true
		}
		g.runs = append(g.runs, job.duration())
		g.waits = append(g.waits, job.wait())
		g.minutes += job.billedMinutes()
	}

	var stats []jobStats
	for _, g := range groups {
		slices.Sort(g.runs)
		slices.Sort(g.waits)
		runsOn := "self-hosted"
		switch {
		case g.hosted && g.selfHosted:
			runsOn = "both"
		case g.hosted:
			runsOn = "GitHub-hosted"
		}
		stats = append(stats, jobStats{
			Workflow:   g.workflow,
			Job:        g.job,
			RunsOn:     runsOn,
			Count:      len(g.runs),
			Minutes:    g.minutes,
			MedianRun:  duration(percentile(g.runs, 0.5)),
			P90Run:     duration(percentile(g.runs, 0.9)),
			MedianWait: duration(percentile(g.waits, 0.5)),
			P90Wait:    duration(percentile(g.waits, 0.9)),
			MaxWait:    duration(percentile(g.waits, 1)),
		})
	}
	sort.Slice(stats, func(a, b int) bool {
		if stats[a].Minutes != stats[b].Minutes {
			return stats[a].Minutes > stats[b].Minutes
		}
		return stats[a].key() < stats[b].key()
	})
	return stats
}

// busiestHours adds up, for each hour on the clock, how long jobs were running
// inside it. Sixty busy minutes in an hour is one runner's worth of work.
func busiestHours(records []jobRecord, loc *time.Location, limit int) []hourStats {
	hours := map[int64]*hourStats{}
	hourOf := func(t time.Time) *hourStats {
		local := t.In(loc)
		start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, loc)
		h, seen := hours[start.Unix()]
		if !seen {
			h = &hourStats{Start: start}
			hours[start.Unix()] = h
		}
		return h
	}

	for _, job := range records {
		if !job.ran() {
			continue
		}
		first := hourOf(job.Started)
		first.JobsStarted++
		first.MaxWait = duration(max(time.Duration(first.MaxWait), job.wait()))

		for t := job.Started; t.Before(job.Finished); {
			h := hourOf(t)
			end := h.Start.Add(time.Hour)
			if job.Finished.Before(end) {
				end = job.Finished
			}
			h.BusyMinutes += end.Sub(t).Minutes()
			t = end
		}
	}

	var busiest []hourStats
	for _, h := range hours {
		busiest = append(busiest, *h)
	}
	sort.Slice(busiest, func(a, b int) bool {
		if busiest[a].BusyMinutes != busiest[b].BusyMinutes {
			return busiest[a].BusyMinutes > busiest[b].BusyMinutes
		}
		return busiest[a].Start.Before(busiest[b].Start)
	})
	if len(busiest) > limit {
		busiest = busiest[:limit]
	}
	return busiest
}

// selfHostedUseOf counts each runner's time from its first job in the window, so
// a runner added part-way through does not read as idle for the days before it
// existed.
func selfHostedUseOf(records []jobRecord, to time.Time) *selfHostedUse {
	firstJob := map[string]time.Time{}
	var busy time.Duration
	for _, job := range records {
		if !job.ran() || job.Hosted {
			continue
		}
		busy += job.duration()
		if first, seen := firstJob[job.Runner]; !seen || job.Started.Before(first) {
			firstJob[job.Runner] = job.Started
		}
	}
	if len(firstJob) == 0 {
		return nil
	}

	var capacity time.Duration
	for _, first := range firstJob {
		capacity += to.Sub(first)
	}
	return &selfHostedUse{Runners: len(firstJob), BusyShare: busy.Seconds() / capacity.Seconds()}
}

// runnerCountFor replays the window's jobs on 1, 2, 3... runners of your own
// and stops at the first pool size that meets the target. It then looks for
// lane candidates on the largest pool that fell short, and sizes each pool again
// with those jobs on a runner of their own. Runs GitHub starts itself stay on
// GitHub's runners, so they are left out.
func runnerCountFor(records []jobRecord, stats []jobStats, opts analysisOptions) (runnerCount, []laneCandidate) {
	var jobs []simJob
	var keys []string
	for _, job := range records {
		if job.ran() && !job.StartedByGitHub {
			jobs = append(jobs, simJob{Arrived: job.Queued, Duration: job.duration()})
			keys = append(keys, job.key())
		}
	}

	result := runnerCount{TargetP90: duration(opts.TargetWait)}
	var waitsByPool [][]time.Duration
	for n := 1; n <= opts.MaxRunners; n++ {
		waits := replay(jobs, n)
		waitsByPool = append(waitsByPool, waits)
		shared := summariseWaits(waits)
		result.Scenarios = append(result.Scenarios, scenario{Runners: n, Shared: shared})
		if time.Duration(shared.P90) <= opts.TargetWait {
			result.Suggested = n
			break
		}
	}

	// When one runner already meets the target there is no runner for a lane
	// to save, so there is nothing to look for.
	result.LaneCheckRunners = len(waitsByPool)
	if result.Suggested > 0 {
		result.LaneCheckRunners = result.Suggested - 1
	}
	if result.LaneCheckRunners == 0 {
		return result, nil
	}
	lanes := laneCandidates(stats, keys, waitsByPool[result.LaneCheckRunners-1], result.LaneCheckRunners)
	if len(lanes) == 0 {
		return result, nil
	}

	inLane := map[string]bool{}
	for _, lane := range lanes {
		inLane[lane.key()] = true
		result.LaneJobs = append(result.LaneJobs, lane.key())
	}
	var laneJobs, otherJobs []simJob
	for i, job := range jobs {
		if inLane[keys[i]] {
			laneJobs = append(laneJobs, job)
		} else {
			otherJobs = append(otherJobs, job)
		}
	}

	laneWaits := replay(laneJobs, 1)
	for i := range result.Scenarios {
		sc := &result.Scenarios[i]
		withLane := summariseWaits(append(slices.Clone(laneWaits), replay(otherJobs, sc.Runners)...))
		sc.WithLane = &withLane
		if result.SuggestedWithLane == 0 && time.Duration(withLane.P90) <= opts.TargetWait {
			result.SuggestedWithLane = sc.Runners
		}
	}
	return result, lanes
}

// laneCandidates picks the quick jobs that, one time in ten, wait longer than
// they run in the replay whose waits are given, one per job named in keys.
func laneCandidates(stats []jobStats, keys []string, waits []time.Duration, runners int) []laneCandidate {
	replayed := map[string][]time.Duration{}
	for i, wait := range waits {
		replayed[keys[i]] = append(replayed[keys[i]], wait)
	}

	var lanes []laneCandidate
	for _, s := range stats {
		jobWaits := replayed[s.key()]
		slices.Sort(jobWaits)
		slowWait := percentile(jobWaits, 0.9)
		if time.Duration(s.MedianRun) <= laneMaxRun && slowWait >= laneMinWait && slowWait > time.Duration(s.P90Run) {
			lanes = append(lanes, laneCandidate{jobStats: s, ReplayRunners: runners, ReplayP90Wait: duration(slowWait)})
		}
	}
	return lanes
}

// cancelledBeforeStart counts runs that were cancelled before any of their jobs
// reached a runner. The default branch comes first and pull requests last: a
// commit on the default branch that is never tested is the loss worth reading
// about, while a pull request's older commit being replaced by its newer one is
// usually the point.
func cancelledBeforeStart(runs []runWithJobs, defaultBranch string) []cancelledRuns {
	type key struct {
		workflow, event string
		onDefault       bool
	}
	counts := map[key]int{}
	for _, r := range runs {
		if r.Run.Conclusion != "cancelled" {
			continue
		}
		started := slices.ContainsFunc(r.Jobs, func(j workflowJob) bool { return j.RunnerName != "" })
		if started {
			continue
		}
		counts[key{r.Run.Name, r.Run.Event, r.Run.HeadBranch == defaultBranch}]++
	}

	var cancelled []cancelledRuns
	for k, n := range counts {
		cancelled = append(cancelled, cancelledRuns{Workflow: k.workflow, Event: k.event, DefaultBranch: k.onDefault, Runs: n})
	}
	sort.Slice(cancelled, func(a, b int) bool {
		x, y := cancelled[a], cancelled[b]
		if x.DefaultBranch != y.DefaultBranch {
			return x.DefaultBranch
		}
		if xPR, yPR := x.Event == "pull_request", y.Event == "pull_request"; xPR != yPR {
			return yPR
		}
		if x.Runs != y.Runs {
			return x.Runs > y.Runs
		}
		return x.Workflow < y.Workflow
	})
	return cancelled
}

func warningsFor(report Report) []string {
	var warnings []string
	if !report.Private {
		warnings = append(warnings, "This repository is public. GitHub's advice is that self-hosted runners should almost never be used "+
			"for public repositories: anyone who can open a pull request can run code on them. "+
			"https://docs.github.com/en/actions/reference/security/secure-use#hardening-for-self-hosted-runners")
	}
	if report.Runs == 0 {
		warnings = append(warnings, "No completed workflow runs in this window.")
	}
	if len(report.Cost.UnpricedLabels) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d GitHub-hosted minutes ran on runners with no known rate (labels: %s), so they are left out of the cost. Price them with --rate LABEL=USD.",
			report.Cost.UnpricedMinutes, strings.Join(report.Cost.UnpricedLabels, "; ")))
	}
	return warnings
}
