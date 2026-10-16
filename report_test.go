package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		24 * time.Second:               "24s",
		8*time.Minute + 14*time.Second: "8m14s",
		15 * time.Minute:               "15m",
		time.Hour + 5*time.Minute:      "1h05m",
		1499 * time.Millisecond:        "1s",
	}
	for d, want := range cases {
		if got := formatDuration(duration(d)); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFormatInt(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"}
	for n, want := range cases {
		if got := formatInt(n); got != want {
			t.Errorf("formatInt(%d) = %q, want %q", n, got, want)
		}
	}
}

func sampleReport() Report {
	runs := []runWithJobs{
		oneRun(
			ranJob("Tests", "suite", 0, 0, mins(8), false),
			ranJob("Pint", "Pint", 0, mins(8), 20*time.Second, false),
			ranJob("Docs", "build", mins(1), 0, mins(2), true),
		),
		{Run: workflowRun{Name: "Tests", Event: "push", HeadBranch: "main", Conclusion: "cancelled"}},
	}
	info := repoInfo{FullName: "acme/widgets", Private: true, DefaultBranch: "main"}
	return analyze(info, runs, t0.Add(-24*time.Hour), t0.Add(time.Hour), analysisOptions{
		Location: time.UTC, TargetWait: time.Minute, MaxRunners: 10,
	})
}

func TestEveryFormatRendersEverySection(t *testing.T) {
	for _, format := range []string{"text", "markdown"} {
		var out bytes.Buffer
		if err := writeReport(&out, sampleReport(), format); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		for _, title := range []string{"Minutes and cost", "Waiting for a runner", "Busiest hours", "How many runners", "Lane candidates", "Cancelled before they started"} {
			if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(title)) {
				t.Errorf("%s output has no %q section", format, title)
			}
		}
	}
}

func TestJSONCarriesDurationsAsSeconds(t *testing.T) {
	var out bytes.Buffer
	if err := writeReport(&out, sampleReport(), "json"); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Jobs []struct {
			Job       string `json:"job"`
			MedianRun int    `json:"median_run_seconds"`
		} `json:"jobs_by_name"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, job := range decoded.Jobs {
		if job.Job == "suite" && job.MedianRun != 480 {
			t.Errorf("suite median run = %d seconds, want 480", job.MedianRun)
		}
	}
}

func TestAPublicRepositoryIsWarnedAboutSelfHostedRunners(t *testing.T) {
	report := sampleReport()
	report.Private = false

	warnings := warningsFor(report)

	if len(warnings) == 0 || !strings.Contains(warnings[0], "public") {
		t.Errorf("warnings = %v, want the public-repository warning first", warnings)
	}
}
