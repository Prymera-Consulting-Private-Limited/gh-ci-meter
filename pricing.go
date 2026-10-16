package main

import (
	"slices"
	"strings"
)

// GitHub's published per-minute rates for its own runners, checked on the date
// below. GitHub bills each job rounded up to the next whole minute. Included
// plan minutes are not subtracted: the report shows list price.
const (
	ratesCheckedOn = "2026-10-17"
	ratesSource    = "https://docs.github.com/en/billing/reference/actions-runner-pricing"
)

// GitHub announced a platform charge of $0.002 a minute for self-hosted
// runners from 1 March 2026, then postponed it before it took effect. Self-hosted
// minutes cost nothing today; the report also shows what they would cost if the
// charge came back, so nobody moves to their own runners believing it is free
// for ever.
const (
	proposedSelfHostedFee = 0.002
	proposedFeeSource     = "https://github.blog/changelog/2025-12-16-coming-soon-simpler-pricing-and-a-better-experience-for-github-actions/"
)

// Standard GitHub-hosted runners cost nothing in a public repository, or for
// Dependabot and GitHub Pages anywhere. Larger runners are always charged.
const freeUsageSource = "https://docs.github.com/en/billing/concepts/product-billing/github-actions"

// The runner group GitHub reports for a job that ran on one of its own runners.
const hostedRunnerGroup = "GitHub Actions"

// rate is one runner's list price. Larger is set for larger runners, which
// GitHub charges for even where standard runners are free. A rate given with
// --rate counts as larger: the table already knows every standard label.
type rate struct {
	Runner       string
	USDPerMinute float64
	Larger       bool
}

// standardRate prices the labels GitHub's standard and macOS larger runners
// answer to. Other larger runners use labels their owners choose, so they can
// only be priced with --rate.
func standardRate(label string) (rate, bool) {
	switch {
	case label == "ubuntu-slim":
		return rate{Runner: "Linux 1-core", USDPerMinute: 0.002}, true
	case strings.HasPrefix(label, "ubuntu-") && strings.HasSuffix(label, "-arm"):
		return rate{Runner: "Linux 2-core arm64", USDPerMinute: 0.005}, true
	case strings.HasPrefix(label, "ubuntu-"):
		return rate{Runner: "Linux 2-core", USDPerMinute: 0.006}, true
	case strings.HasPrefix(label, "windows-"):
		return rate{Runner: "Windows 2-core", USDPerMinute: 0.010}, true
	case strings.HasPrefix(label, "macos-") && strings.HasSuffix(label, "-xlarge"):
		return rate{Runner: "macOS 5-core (M2 Pro)", USDPerMinute: 0.102, Larger: true}, true
	case strings.HasPrefix(label, "macos-") && strings.HasSuffix(label, "-large"):
		return rate{Runner: "macOS 12-core", USDPerMinute: 0.077, Larger: true}, true
	case strings.HasPrefix(label, "macos-"):
		return rate{Runner: "macOS 3- or 4-core", USDPerMinute: 0.062}, true
	}
	return rate{}, false
}

// hostedRate prices a GitHub-hosted job from the labels it asked for. A rate
// given on the command line wins over the built-in table.
func hostedRate(labels []string, overrides map[string]float64) (rate, bool) {
	for _, label := range labels {
		if usd, ok := overrides[label]; ok {
			return rate{Runner: label, USDPerMinute: usd, Larger: true}, true
		}
	}
	for _, label := range labels {
		if r, ok := standardRate(label); ok {
			return r, true
		}
	}
	return rate{}, false
}

// isHosted says whether a job ran, or would have run, on GitHub's runners. A
// job that ran is told by its runner group. One that never reached a runner is
// judged by its labels.
func isHosted(job workflowJob) bool {
	if job.RunnerName != "" {
		return job.RunnerGroupName == hostedRunnerGroup
	}
	if slices.Contains(job.Labels, "self-hosted") {
		return false
	}
	for _, label := range job.Labels {
		if _, ok := standardRate(label); ok {
			return true
		}
	}
	return false
}
