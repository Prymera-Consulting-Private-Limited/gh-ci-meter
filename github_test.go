package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRunListing answers GitHub's run listing from a fixed set of runs, with
// GitHub's own limit: total_count says how many match, but no more than
// runListingLimit can be paged through.
type fakeRunListing struct {
	runs     []workflowRun
	requests int
}

func (f *fakeRunListing) Get(path string, response interface{}) error {
	f.requests++
	_, rawQuery, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}
	fromText, toText, _ := strings.Cut(query.Get("created"), "..")
	from, err := time.Parse(time.RFC3339, fromText)
	if err != nil {
		return err
	}
	to, err := time.Parse(time.RFC3339, toText)
	if err != nil {
		return err
	}
	page, _ := strconv.Atoi(query.Get("page"))
	perPage, _ := strconv.Atoi(query.Get("per_page"))

	var matching []workflowRun
	for _, run := range f.runs {
		if !run.CreatedAt.Before(from) && !run.CreatedAt.After(to) {
			matching = append(matching, run)
		}
	}
	reachable := matching[:min(len(matching), runListingLimit)]
	start := min((page-1)*perPage, len(reachable))
	end := min(start+perPage, len(reachable))

	body, err := json.Marshal(map[string]any{
		"total_count":   len(matching),
		"workflow_runs": reachable[start:end],
	})
	if err != nil {
		return err
	}
	return json.Unmarshal(body, response)
}

func TestARangeWithMoreRunsThanGitHubWillListIsSplit(t *testing.T) {
	// 2,500 runs in one day, a run every 30 seconds from midnight.
	day := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	fake := &fakeRunListing{}
	for i := range 2500 {
		fake.runs = append(fake.runs, workflowRun{
			ID:        int64(i + 1),
			Status:    "completed",
			CreatedAt: day.Add(time.Duration(i) * 30 * time.Second),
		})
	}
	f := &fetcher{client: fake, repo: "acme/widgets", progress: func(string, ...any) {}}

	runs, err := f.completedRuns(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != 2500 {
		t.Fatalf("listed %d runs, want all 2500", len(runs))
	}
	seen := map[int64]bool{}
	for _, run := range runs {
		if seen[run.ID] {
			t.Fatalf("run %d listed twice", run.ID)
		}
		seen[run.ID] = true
	}
}

func TestOnlyCompletedRunsAreKept(t *testing.T) {
	day := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	fake := &fakeRunListing{runs: []workflowRun{
		{ID: 1, Status: "completed", CreatedAt: day.Add(time.Hour)},
		{ID: 2, Status: "in_progress", CreatedAt: day.Add(2 * time.Hour)},
		{ID: 3, Status: "queued", CreatedAt: day.Add(3 * time.Hour)},
	}}
	f := &fetcher{client: fake, repo: "acme/widgets", progress: func(string, ...any) {}}

	runs, err := f.completedRuns(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != 1 || runs[0].ID != 1 {
		t.Errorf("runs = %v, want only the completed one", runs)
	}
}

func TestRateFlag(t *testing.T) {
	rates := rateFlag{}
	if err := rates.Set("linux-8-core=0.022"); err != nil {
		t.Fatal(err)
	}
	if rates["linux-8-core"] != 0.022 {
		t.Errorf("rates = %v", rates)
	}
	for _, bad := range []string{"linux-8-core", "=0.1", "linux-8-core=cheap", "linux-8-core=-1"} {
		if err := rates.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
}

func TestRepositoryMayComeBeforeOrAfterTheFlags(t *testing.T) {
	for _, args := range [][]string{
		{"acme/widgets", "--days", "7"},
		{"--days", "7", "acme/widgets"},
	} {
		opts, err := parseOptions(args, &strings.Builder{})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if opts.Repo != "acme/widgets" || opts.Days != 7 {
			t.Errorf("%v: repo %q, days %d", args, opts.Repo, opts.Days)
		}
	}
	if _, err := parseOptions([]string{"acme/widgets", "acme/other"}, &strings.Builder{}); err == nil {
		t.Error("two repositories accepted")
	}
	if _, err := parseOptions([]string{"--format", "xml"}, &strings.Builder{}); err == nil {
		t.Error("format xml accepted")
	}
}
