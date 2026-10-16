package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// GitHub answers a filtered run listing with at most this many runs, however
// many pages are requested. A time range holding more is split in half until
// each part fits.
const runListingLimit = 1000

// GitHub asks API clients not to send many requests at once. A few at a time
// keeps a 30-day fetch to minutes without tripping its secondary rate limit.
const jobFetchWorkers = 4

const pageSize = 100

type workflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	HeadBranch string    `json:"head_branch"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunAttempt int       `json:"run_attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

type workflowJob struct {
	ID              int64     `json:"id"`
	RunID           int64     `json:"run_id"`
	Name            string    `json:"name"`
	WorkflowName    string    `json:"workflow_name"`
	Status          string    `json:"status"`
	Conclusion      string    `json:"conclusion"`
	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	Labels          []string  `json:"labels"`
	RunnerName      string    `json:"runner_name"`
	RunnerGroupName string    `json:"runner_group_name"`
}

type repoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

type runWithJobs struct {
	Run  workflowRun
	Jobs []workflowJob
}

// restGetter is the one call the fetcher makes. gh's REST client satisfies it,
// and so can a fake GitHub in the tests.
type restGetter interface {
	Get(path string, response interface{}) error
}

type fetcher struct {
	client   restGetter
	repo     string // OWNER/REPO
	cacheDir string // empty when the cache is switched off
	progress func(format string, args ...any)
}

func (f *fetcher) repoInfo() (repoInfo, error) {
	var info repoInfo
	err := f.client.Get(fmt.Sprintf("repos/%s", f.repo), &info)
	return info, err
}

// completedRuns lists the runs created in [from, to) that have finished.
// Runs still going are left out: their jobs are not final yet.
func (f *fetcher) completedRuns(from, to time.Time) ([]workflowRun, error) {
	var completed []workflowRun
	for day := from; day.Before(to); day = day.Add(24 * time.Hour) {
		end := day.Add(24 * time.Hour)
		if end.After(to) {
			end = to
		}
		runs, err := f.runsBetween(day, end)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			if run.Status == "completed" {
				completed = append(completed, run)
			}
		}
		f.progress("Listing runs up to %s: %d so far", day.Format("2 Jan"), len(completed))
	}
	return completed, nil
}

func (f *fetcher) runsBetween(from, to time.Time) ([]workflowRun, error) {
	created := fmt.Sprintf("%s..%s",
		from.UTC().Format(time.RFC3339),
		to.Add(-time.Second).UTC().Format(time.RFC3339))

	var runs []workflowRun
	for page := 1; ; page++ {
		query := url.Values{
			"created":  {created},
			"per_page": {strconv.Itoa(pageSize)},
			"page":     {strconv.Itoa(page)},
		}
		var response struct {
			TotalCount   int           `json:"total_count"`
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		path := fmt.Sprintf("repos/%s/actions/runs?%s", f.repo, query.Encode())
		if err := f.client.Get(path, &response); err != nil {
			return nil, err
		}

		if page == 1 && response.TotalCount > runListingLimit && to.Sub(from) > 2*time.Second {
			middle := from.Add(to.Sub(from) / 2).Truncate(time.Second)
			first, err := f.runsBetween(from, middle)
			if err != nil {
				return nil, err
			}
			second, err := f.runsBetween(middle, to)
			if err != nil {
				return nil, err
			}
			return append(first, second...), nil
		}

		runs = append(runs, response.WorkflowRuns...)
		if len(response.WorkflowRuns) < pageSize {
			return runs, nil
		}
	}
}

// jobsOf fetches the jobs of every run, a few runs at a time. It stops handing
// out work at the first error and returns that error.
func (f *fetcher) jobsOf(runs []workflowRun) ([]runWithJobs, error) {
	results := make([]runWithJobs, len(runs))
	work := make(chan int)

	var mu sync.Mutex
	var firstErr error
	fetched := 0

	var wg sync.WaitGroup
	for range jobFetchWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				jobs, err := f.runJobs(runs[i])

				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				results[i] = runWithJobs{Run: runs[i], Jobs: jobs}
				fetched++
				if fetched%25 == 0 || fetched == len(runs) {
					f.progress("Reading jobs: %d of %d runs", fetched, len(runs))
				}
				mu.Unlock()
			}
		}()
	}

	for i := range runs {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()

	return results, firstErr
}

// runJobs returns every job of a run, re-runs included. A finished run's jobs
// never change, so they are cached by run and attempt and a second report over
// the same days costs no API calls.
func (f *fetcher) runJobs(run workflowRun) ([]workflowJob, error) {
	cacheFile := f.cacheFile(run)
	if jobs, ok := readCachedJobs(cacheFile); ok {
		return jobs, nil
	}

	var jobs []workflowJob
	for page := 1; ; page++ {
		var response struct {
			Jobs []workflowJob `json:"jobs"`
		}
		path := fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=all&per_page=%d&page=%d",
			f.repo, run.ID, pageSize, page)
		if err := f.client.Get(path, &response); err != nil {
			return nil, err
		}
		jobs = append(jobs, response.Jobs...)
		if len(response.Jobs) < pageSize {
			break
		}
	}

	writeCachedJobs(cacheFile, jobs)
	return jobs, nil
}

func (f *fetcher) cacheFile(run workflowRun) string {
	if f.cacheDir == "" {
		return ""
	}
	name := fmt.Sprintf("run-%d-attempt-%d.json", run.ID, run.RunAttempt)
	return filepath.Join(f.cacheDir, filepath.FromSlash(f.repo), name)
}

func readCachedJobs(path string) ([]workflowJob, bool) {
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var jobs []workflowJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, false
	}
	return jobs, true
}

// writeCachedJobs ignores failures: the cache only saves API calls, and a
// report should not fail because a cache directory is read-only.
func writeCachedJobs(path string, jobs []workflowJob) {
	if path == "" {
		return
	}
	data, err := json.Marshal(jobs)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
