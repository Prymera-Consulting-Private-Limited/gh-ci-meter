package main

import "testing"

func TestStandardRates(t *testing.T) {
	cases := map[string]float64{
		"ubuntu-latest":    0.006,
		"ubuntu-24.04":     0.006,
		"ubuntu-24.04-arm": 0.005,
		"ubuntu-slim":      0.002,
		"windows-latest":   0.010,
		"windows-11-arm":   0.010,
		"macos-latest":     0.062,
		"macos-15-large":   0.077,
		"macos-15-xlarge":  0.102,
	}
	for label, want := range cases {
		got, ok := standardRate(label)
		if !ok || got.USDPerMinute != want {
			t.Errorf("%s: rate = %v (%v), want %v", label, got.USDPerMinute, ok, want)
		}
	}
	if _, ok := standardRate("linux-8-core"); ok {
		t.Error("a larger runner's own label must not be priced from the table")
	}
}

func TestIsHosted(t *testing.T) {
	cases := []struct {
		name string
		job  workflowJob
		want bool
	}{
		{"ran on GitHub", workflowJob{RunnerName: "GitHub Actions 3", RunnerGroupName: hostedRunnerGroup, Labels: []string{"ubuntu-latest"}}, true},
		{"ran on our own", workflowJob{RunnerName: "build-box", RunnerGroupName: "Default", Labels: []string{"self-hosted"}}, false},
		{"lane runner without default labels", workflowJob{RunnerName: "lint-box", RunnerGroupName: "Default", Labels: []string{"lint"}}, false},
		{"never ran, hosted label", workflowJob{Labels: []string{"ubuntu-latest"}}, true},
		{"never ran, self-hosted label", workflowJob{Labels: []string{"self-hosted", "macOS"}}, false},
	}
	for _, c := range cases {
		if got := isHosted(c.job); got != c.want {
			t.Errorf("%s: isHosted = %v, want %v", c.name, got, c.want)
		}
	}
}
