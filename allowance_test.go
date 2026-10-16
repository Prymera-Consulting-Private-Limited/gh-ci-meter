package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestPlanFetchFitsWhenTheReserveIsLeftOver(t *testing.T) {
	left := allowance{Remaining: 4000, Limit: 5000}

	if plan := planFetch(3500, 30, left); !plan.Fits {
		t.Errorf("3,500 calls with 4,000 left and 500 kept back: %+v, want it to fit", plan)
	}
	if plan := planFetch(3501, 30, left); plan.Fits {
		t.Errorf("3,501 calls would eat into the reserve: %+v, want it refused", plan)
	}
}

func TestPlanFetchSuggestsDaysThatFitAndCountsTheResetsToWaitFor(t *testing.T) {
	// A big repository: 1,500 runs a day for 30 days.
	plan := planFetch(45000, 30, allowance{Remaining: 4500, Limit: 5000})

	if plan.Fits {
		t.Fatal("45,000 calls cannot fit in one hour's allowance")
	}
	// 4,000 calls after the reserve, at 1,500 a day: two days.
	if plan.DaysToFit != 2 {
		t.Errorf("days to fit = %d, want 2", plan.DaysToFit)
	}
	// 40,500 calls beyond what is left, at 5,000 an hour.
	if plan.WaitResets != 9 {
		t.Errorf("resets to wait for = %d, want 9", plan.WaitResets)
	}
	if !strings.Contains(plan.refusal(allowance{Remaining: 4500, Limit: 5000}), "--days 2") {
		t.Errorf("refusal does not suggest --days 2: %s", plan.refusal(allowance{Remaining: 4500, Limit: 5000}))
	}
}

func TestPlanFetchWhenNotEvenOneDayFits(t *testing.T) {
	plan := planFetch(30000, 10, allowance{Remaining: 600, Limit: 5000})

	if plan.DaysToFit != 0 {
		t.Errorf("days to fit = %d, want 0", plan.DaysToFit)
	}
	if !strings.Contains(plan.refusal(allowance{Remaining: 600, Limit: 5000}), "--wait") {
		t.Error("with no --days that fits, the refusal must point at --wait")
	}
}

func limitError(status int, headers map[string]string, message string) error {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &api.HTTPError{StatusCode: status, Headers: h, Message: message}
}

func TestRateLimitPauseFollowsGitHubsOrder(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	reset := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)

	cases := []struct {
		name    string
		err     error
		pause   time.Duration
		limited bool
	}{
		{"retry-after wins", limitError(403, map[string]string{"Retry-After": "30", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, ""), 30 * time.Second, true},
		{"primary limit waits for the reset", limitError(429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, ""), 10*time.Minute + 5*time.Second, true},
		{"secondary limit with no headers waits a minute", limitError(403, nil, "You have exceeded a secondary rate limit"), time.Minute, true},
		{"a refusal that is not a limit", limitError(403, nil, "Resource not accessible by integration"), 0, false},
		{"not found", limitError(404, nil, "Not Found"), 0, false},
		{"not an HTTP error", errors.New("connection reset"), 0, false},
		{"no error", nil, 0, false},
	}
	for _, c := range cases {
		pause, limited := rateLimitPause(c.err, now)
		if pause != c.pause || limited != c.limited {
			t.Errorf("%s: pause %v, limited %v; want %v, %v", c.name, pause, limited, c.pause, c.limited)
		}
	}
}

// limitedOnce refuses the first request with a rate limit, then answers.
type limitedOnce struct{ calls int }

func (l *limitedOnce) Get(path string, response interface{}) error {
	l.calls++
	if l.calls == 1 {
		return limitError(403, map[string]string{"Retry-After": "42"}, "")
	}
	return nil
}

// failsFirst answers its first few requests with a server error.
type failsFirst struct{ failures, calls int }

func (f *failsFirst) Get(path string, response interface{}) error {
	f.calls++
	if f.calls <= f.failures {
		return limitError(502, nil, "Server Error")
	}
	return nil
}

func TestPacedClientRidesOutAFewServerErrors(t *testing.T) {
	var slept time.Duration
	paced := &pacedClient{
		client: &failsFirst{failures: 2},
		notify: func(string, ...any) {},
		sleep:  func(d time.Duration) { slept += d },
		now:    time.Now,
	}
	if err := paced.Get("repos/acme/widgets", nil); err != nil {
		t.Fatalf("two 502s then an answer: %v", err)
	}
	if slept != 6*time.Second {
		t.Errorf("slept %v, want 2s then 4s", slept)
	}

	persistent := &pacedClient{client: &failsFirst{failures: 10}, notify: func(string, ...any) {}, sleep: func(time.Duration) {}, now: time.Now}
	if err := persistent.Get("repos/acme/widgets", nil); err == nil {
		t.Error("a server that keeps failing must end in an error, not retry for ever")
	}
}

func TestPacedClientWaitsAndTriesAgainOnlyWhenAskedTo(t *testing.T) {
	var slept time.Duration
	paced := &pacedClient{
		client: &limitedOnce{},
		wait:   true,
		notify: func(string, ...any) {},
		sleep:  func(d time.Duration) { slept += d },
		now:    time.Now,
	}
	if err := paced.Get("repos/acme/widgets", nil); err != nil {
		t.Fatalf("with --wait: %v", err)
	}
	if slept != 42*time.Second {
		t.Errorf("slept %v, want the 42s GitHub asked for", slept)
	}

	impatient := &pacedClient{client: &limitedOnce{}, notify: func(string, ...any) {}, sleep: func(time.Duration) {}, now: time.Now}
	err := impatient.Get("repos/acme/widgets", nil)
	if err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Errorf("without --wait: %v, want an error that mentions --wait", err)
	}
}
