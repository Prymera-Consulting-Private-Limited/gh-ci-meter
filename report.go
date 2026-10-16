package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// The jobs table lists the jobs that used the most minutes; --format json
// carries every one.
const jobsShown = 15

// section is one part of the report, written once and rendered as text or
// Markdown.
type section struct {
	Title string
	Lines []string
	Table *table
	Notes []string
}

type table struct {
	Header []string
	Rows   [][]string
}

func writeReport(w io.Writer, report Report, format string) error {
	switch format {
	case "json":
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case "markdown":
		return writeMarkdown(w, report)
	default:
		return writeText(w, report)
	}
}

func writeText(w io.Writer, report Report) error {
	fmt.Fprintf(w, "%s\n%s\n", report.Repo, headline(report))
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "! %s\n", warning)
	}

	for _, s := range sections(report) {
		fmt.Fprintf(w, "\n%s\n%s\n", strings.ToUpper(s.Title), strings.Repeat("-", len(s.Title)))
		for _, line := range s.Lines {
			fmt.Fprintln(w, line)
		}
		if s.Table != nil {
			fmt.Fprintln(w)
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, strings.Join(s.Table.Header, "\t"))
			for _, row := range s.Table.Rows {
				fmt.Fprintln(tw, strings.Join(row, "\t"))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
		if len(s.Notes) > 0 {
			fmt.Fprintln(w)
		}
		for _, note := range s.Notes {
			fmt.Fprintf(w, "  * %s\n", note)
		}
	}
	return nil
}

func writeMarkdown(w io.Writer, report Report) error {
	fmt.Fprintf(w, "# CI report: %s\n\n%s\n", report.Repo, headline(report))
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "\n> **Warning.** %s\n", warning)
	}

	for _, s := range sections(report) {
		fmt.Fprintf(w, "\n## %s\n", s.Title)
		for _, line := range s.Lines {
			fmt.Fprintf(w, "\n%s\n", line)
		}
		if s.Table != nil {
			fmt.Fprintf(w, "\n| %s |\n", strings.Join(s.Table.Header, " | "))
			fmt.Fprintf(w, "|%s\n", strings.Repeat("---|", len(s.Table.Header)))
			for _, row := range s.Table.Rows {
				fmt.Fprintf(w, "| %s |\n", strings.Join(escapeCells(row), " | "))
			}
		}
		if len(s.Notes) > 0 {
			fmt.Fprintln(w)
			for _, note := range s.Notes {
				fmt.Fprintf(w, "- %s\n", note)
			}
		}
	}
	return nil
}

func escapeCells(row []string) []string {
	escaped := make([]string, len(row))
	for i, cell := range row {
		escaped[i] = strings.ReplaceAll(cell, "|", `\|`)
	}
	return escaped
}

func headline(report Report) string {
	days := fmt.Sprintf("%s days", trimFloat(report.Cost.Days))
	if trimFloat(report.Cost.Days) == "1" {
		days = "1 day"
	}
	return fmt.Sprintf("%s to %s (%s), times in %s. %s completed runs, %s jobs.",
		report.From.In(locationOf(report)).Format("2 Jan 2006 15:04"),
		report.To.In(locationOf(report)).Format("2 Jan 2006 15:04"),
		days,
		report.TimeZone,
		formatInt(report.Runs),
		formatInt(report.Jobs))
}

func locationOf(report Report) *time.Location {
	if report.location == nil {
		return time.UTC
	}
	return report.location
}

func sections(report Report) []section {
	return []section{
		costSection(report),
		waitSection(report),
		busySection(report),
		runnerCountSection(report),
		laneSection(report),
		cancelledSection(report),
	}
}

func costSection(report Report) section {
	cost := report.Cost
	per30 := 30 / cost.Days
	s := section{Title: "1. Minutes and cost"}

	if cost.HostedMinutes > 0 {
		summary := fmt.Sprintf("GitHub-hosted: %s billed minutes, %s at list price%s.",
			formatInt(cost.HostedMinutes), formatUSD(cost.HostedUSD), perThirtyDays(cost.HostedUSD, cost.Days))
		if cost.FreeMinutes > 0 {
			summary += fmt.Sprintf(" %s of those minutes cost nothing.", formatInt(cost.FreeMinutes))
		}
		s.Lines = append(s.Lines, summary)
		s.Table = &table{Header: []string{"Runner", "Rate", "Jobs", "Minutes", "Cost"}}
		for _, line := range cost.Hosted {
			runner := line.Runner
			if line.FreeBecause != "" {
				runner = fmt.Sprintf("%s (free: %s)", line.Runner, line.FreeBecause)
			}
			s.Table.Rows = append(s.Table.Rows, []string{
				runner,
				fmt.Sprintf("$%.3f/min", line.USDPerMinute),
				formatInt(line.Jobs),
				formatInt(line.Minutes),
				formatUSD(line.USD),
			})
		}
	} else {
		s.Lines = append(s.Lines, "GitHub-hosted: none in this window.")
	}

	if cost.SelfHostedMinutes > 0 {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"Self-hosted: %s minutes. $0 today; %s%s if GitHub's postponed $%.3f/min self-hosted charge came back.",
			formatInt(cost.SelfHostedMinutes), formatUSD(cost.SelfHostedAtFeeUSD),
			perThirtyDays(cost.SelfHostedAtFeeUSD, cost.Days), cost.ProposedFeePerMinute))
	}

	if cost.StillHostedUSD > 0 {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"Moving the GitHub-hosted jobs you still pay for to your own runners would save about %s per 30 days today, or %s if the self-hosted charge came back, provided they run as fast there.",
			formatUSD(cost.StillHostedUSD*per30), formatUSD((cost.StillHostedUSD-cost.StillHostedAtFeeUSD)*per30)))
	}
	if len(cost.MovedJobs) > 0 {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"Already moved to your own runners: %s. Their runs on GitHub's runners earlier in this window cost %s at list price, and that spending has stopped.",
			listOf(cost.MovedJobs, 3), formatUSD(cost.MovedUSD)))
	}

	s.Notes = []string{
		fmt.Sprintf("List prices from GitHub, checked %s: %s", cost.RatesCheckedOn, cost.RatesSource),
		"Each job is rounded up to a whole minute, as GitHub bills it. Minutes your plan includes are not subtracted.",
		fmt.Sprintf("Standard runners are free in a public repository and for Dependabot and GitHub Pages; larger runners never are: %s", cost.FreeUsageSource),
		fmt.Sprintf("The postponed self-hosted charge: %s", cost.ProposedFeeSource),
	}
	return s
}

// perThirtyDays scales a window's cost to 30 days, and says nothing when the
// window already is 30 days.
func perThirtyDays(usd, days float64) string {
	if math.Abs(days-30) < 0.5 {
		return ""
	}
	return fmt.Sprintf(" (%s per 30 days)", formatUSD(usd*30/days))
}

func waitSection(report Report) section {
	s := section{
		Title: "2. Waiting for a runner, and running",
		Lines: []string{"Wait is the time from GitHub queueing a job to a runner picking it up. A wait longer than the run means the job sat in a queue."},
	}
	if len(report.JobStats) == 0 {
		s.Lines = append(s.Lines, "No job reached a runner in this window.")
		return s
	}

	s.Table = &table{Header: []string{"Workflow / job", "Runs on", "Jobs", "Minutes", "Run (median, p90)", "Wait (median, p90, max)"}}
	for i, stats := range report.JobStats {
		if i == jobsShown {
			break
		}
		s.Table.Rows = append(s.Table.Rows, []string{
			shorten(stats.key(), 48),
			stats.RunsOn,
			formatInt(stats.Count),
			formatInt(stats.Minutes),
			fmt.Sprintf("%s, %s", formatDuration(stats.MedianRun), formatDuration(stats.P90Run)),
			fmt.Sprintf("%s, %s, %s", formatDuration(stats.MedianWait), formatDuration(stats.P90Wait), formatDuration(stats.MaxWait)),
		})
	}
	if hidden := len(report.JobStats) - jobsShown; hidden > 0 {
		s.Notes = append(s.Notes, fmt.Sprintf("%d more jobs used fewer minutes and are not shown; --format json lists every one.", hidden))
	}
	return s
}

func busySection(report Report) section {
	s := section{
		Title: "3. Busiest hours",
		Lines: []string{"Busy runner-minutes add up the time jobs were running inside each hour. Sixty is one runner busy for the whole hour."},
	}
	if use := report.SelfHosted; use != nil {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"Your %d self-hosted runner(s) were busy %.0f%% of the time. Work comes in bursts, so the busiest hours say more than the average.",
			use.Runners, use.BusyShare*100))
		s.Notes = append(s.Notes, "Each runner is counted from its first job in the window. One removed part-way through reads as idle from then on.")
	}
	if len(report.BusiestHours) == 0 {
		return s
	}

	s.Table = &table{Header: []string{"Hour", "Busy runner-minutes", "Runners' worth", "Jobs started", "Longest wait"}}
	for _, h := range report.BusiestHours {
		s.Table.Rows = append(s.Table.Rows, []string{
			h.Start.Format("Mon 2 Jan 15:00"),
			fmt.Sprintf("%.0f", h.BusyMinutes),
			fmt.Sprintf("%.1f", h.BusyMinutes/60),
			formatInt(h.JobsStarted),
			formatDuration(h.MaxWait),
		})
	}
	return s
}

func runnerCountSection(report Report) section {
	count := report.RunnerCount
	s := section{
		Title: "4. How many runners",
		Lines: []string{"The window's jobs replayed in the order they were queued, on a pool of your own runners that each take one job at a time. " +
			"Dependabot, GitHub Pages and other runs GitHub starts itself are left out: they stay on GitHub's runners whatever your workflows ask for."},
	}

	header := []string{"Runners", "Median wait", "p90 wait", "Longest wait", "Waits over 5m"}
	hasLane := len(count.LaneJobs) > 0
	if hasLane {
		header = append(header, "With a lane: p90, longest")
	}
	s.Table = &table{Header: header}
	for _, sc := range count.Scenarios {
		row := []string{
			fmt.Sprintf("%d", sc.Runners),
			formatDuration(sc.Shared.Median),
			formatDuration(sc.Shared.P90),
			formatDuration(sc.Shared.Max),
			formatInt(sc.Shared.OverLimit),
		}
		if hasLane && sc.WithLane != nil {
			row = append(row, fmt.Sprintf("%s, %s", formatDuration(sc.WithLane.P90), formatDuration(sc.WithLane.Max)))
		}
		s.Table.Rows = append(s.Table.Rows, row)
	}

	target := formatDuration(count.TargetP90)
	if count.Suggested > 0 {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"Fewest runners that keep 9 jobs in 10 waiting no more than %s: %d.", target, count.Suggested))
	} else {
		s.Lines = append(s.Lines, fmt.Sprintf(
			"No pool in the table keeps 9 jobs in 10 waiting no more than %s. Raise --max-runners, or look for jobs that could run less often.", target))
	}
	if hasLane {
		if count.SuggestedWithLane > 0 {
			s.Lines = append(s.Lines, fmt.Sprintf(
				"With the lane candidates (section 5) on a runner of their own: %d, plus the lane runner.", count.SuggestedWithLane))
		}
		s.Notes = append(s.Notes, "\"With a lane\" adds one runner that takes only the lane candidates; the runner count is for everything else.")
	}
	s.Notes = append(s.Notes,
		"Assumes your runners run each job as fast as it ran here. A different machine will be faster or slower.",
		"Ignores concurrency groups and needs: between jobs, which make some jobs wait whatever the pool size.")
	return s
}

func laneSection(report Report) section {
	s := section{Title: "5. Lane candidates"}
	quick := formatDuration(duration(laneMaxRun))
	checked := report.RunnerCount.LaneCheckRunners
	switch {
	case checked == 0:
		s.Lines = []string{"None needed: one runner of your own already meets the wait target, so a lane has no runner to save."}
		return s
	case len(report.Lanes) == 0:
		s.Lines = []string{fmt.Sprintf(
			"None: replayed on %d runner(s), one fewer than a shared pool needs, no job that runs in under %s waits longer than it runs.", checked, quick)}
		return s
	}

	s.Lines = []string{fmt.Sprintf(
		"Replayed on %d runner(s), one fewer than a shared pool needs, these jobs run in under %s but one time in ten wait longer than they run, behind longer jobs. "+
			"A runner that takes only them removes the wait and lets the rest share fewer runners (section 4): give it a label of its own "+
			"(register it with --no-default-labels --labels <lane>) and point the jobs' runs-on at that label.", checked, quick)}
	s.Notes = []string{"The replay puts every job in one pool, so a job that already has a lane of its own is listed too."}
	s.Table = &table{Header: []string{"Workflow / job", "Jobs", "Median run", fmt.Sprintf("p90 wait on %d runner(s)", checked), "p90 wait as it ran"}}
	for _, lane := range report.Lanes {
		s.Table.Rows = append(s.Table.Rows, []string{
			shorten(lane.key(), 48),
			formatInt(lane.Count),
			formatDuration(lane.MedianRun),
			formatDuration(lane.ReplayP90Wait),
			formatDuration(lane.P90Wait),
		})
	}
	return s
}

func cancelledSection(report Report) section {
	s := section{Title: "6. Cancelled before they started"}
	if len(report.Cancelled) == 0 {
		s.Lines = []string{"None: every cancelled run had at least one job reach a runner."}
		return s
	}

	s.Lines = []string{"Runs cancelled before any of their jobs reached a runner. The usual cause is a concurrency group: " +
		"it holds one running and one waiting run, and a newer run replaces the waiting one, even with cancel-in-progress: false. " +
		"A manual cancel looks the same; GitHub does not record which it was."}
	onDefault := slices.ContainsFunc(report.Cancelled, func(c cancelledRuns) bool { return c.DefaultBranch })
	onPullRequests := slices.ContainsFunc(report.Cancelled, func(c cancelledRuns) bool { return c.Event == "pull_request" })
	if onDefault {
		s.Lines = append(s.Lines, "On the default branch it matters: the commit in between is never tested on its own, "+
			"even though the newer run tests a branch that contains it.")
	}
	if onPullRequests {
		s.Lines = append(s.Lines, "Pull-request rows are normally harmless: a newer push to the same pull request replaced an older one, "+
			"and only its latest commit needs testing.")
	}
	s.Table = &table{Header: []string{"Workflow", "Event", "Branch", "Runs"}}
	for _, c := range report.Cancelled {
		branch := "other"
		if c.DefaultBranch {
			branch = fmt.Sprintf("%s (default)", report.DefaultBranch)
		}
		s.Table.Rows = append(s.Table.Rows, []string{c.Workflow, c.Event, branch, formatInt(c.Runs)})
	}
	return s
}

func formatDuration(d duration) string {
	t := time.Duration(d).Round(time.Second)
	switch {
	case t < time.Minute:
		return fmt.Sprintf("%ds", int(t.Seconds()))
	case t < time.Hour:
		minutes, seconds := int(t.Minutes()), int(t.Seconds())%60
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%dh%02dm", int(t.Hours()), int(t.Minutes())%60)
	}
}

func formatInt(n int) string {
	digits := fmt.Sprintf("%d", n)
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return grouped.String()
}

func formatUSD(usd float64) string {
	return fmt.Sprintf("$%.2f", usd)
}

func trimFloat(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}

// listOf names the first few items and counts the rest.
func listOf(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:limit], ", "), len(items)-limit)
}

func shorten(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}
