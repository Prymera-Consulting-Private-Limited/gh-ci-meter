package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// allowance is what is left of the hourly API allowance gh's login has. Every
// gh command and extension on the machine draws on the same one.
type allowance struct {
	Remaining int
	Limit     int
	Reset     time.Time
}

func (f *fetcher) allowance() (allowance, error) {
	var response struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := f.client.Get("rate_limit", &response); err != nil {
		return allowance{}, err
	}
	core := response.Resources.Core
	return allowance{Remaining: core.Remaining, Limit: core.Limit, Reset: time.Unix(core.Reset, 0)}, nil
}

// A report that would leave less than this share of the hourly allowance stops
// and says so, rather than leaving the user's other gh commands with nothing.
const reservedShare = 0.1

// fetchPlan says, before any jobs are read, whether a report fits in what is
// left of the allowance, and what to do if it does not.
type fetchPlan struct {
	Calls      int // about one per run whose jobs are not cached yet
	Budget     int // calls the report may use and still leave the reserve
	Fits       bool
	DaysToFit  int // a --days that would fit now, 0 when even one day would not
	WaitResets int // how many more hourly resets --wait would sit through
}

func planFetch(uncachedRuns, days int, left allowance) fetchPlan {
	reserve := int(math.Ceil(float64(left.Limit) * reservedShare))
	plan := fetchPlan{Calls: uncachedRuns, Budget: max(left.Remaining-reserve, 0)}
	plan.Fits = plan.Calls <= plan.Budget
	if plan.Fits {
		return plan
	}

	perDay := float64(uncachedRuns) / float64(days)
	plan.DaysToFit = min(int(float64(plan.Budget)/perDay), days-1)
	if left.Limit > 0 {
		plan.WaitResets = int(math.Ceil(float64(plan.Calls-left.Remaining) / float64(left.Limit)))
	}
	return plan
}

func (p fetchPlan) refusal(left allowance) string {
	var advice string
	switch {
	case p.DaysToFit > 0:
		advice = fmt.Sprintf("Run with --days %d to fit, or add --wait", p.DaysToFit)
	default:
		advice = "Even one day needs more than that. Add --wait"
	}
	return fmt.Sprintf(
		"this report needs about %s API calls, one for each run not read before, and GitHub allows %s more until %s, "+
			"of which %s are kept back for your other gh commands. %s to read at GitHub's pace: about %d more hour(s), "+
			"during which other gh commands may be refused",
		formatInt(p.Calls), formatInt(left.Remaining), left.Reset.Local().Format("15:04"),
		formatInt(left.Remaining-p.Budget), advice, max(p.WaitResets, 1))
}

// pacedClient answers GitHub's rate limits. Without --wait it turns them into
// an error that says when to come back; with it, it waits as GitHub asks and
// tries again.
type pacedClient struct {
	client restGetter
	wait   bool
	notify func(format string, args ...any)
	sleep  func(time.Duration)
	now    func() time.Time
}

// GitHub answers a busy endpoint with a 502 or 503 now and then. A few tries,
// further apart each time, ride those out instead of losing a long report to one.
const serverErrorRetries = 3

func (p *pacedClient) Get(path string, response interface{}) error {
	serverErrors := 0
	for {
		err := p.client.Get(path, response)
		if isServerError(err) && serverErrors < serverErrorRetries {
			serverErrors++
			p.sleep(time.Duration(1<<serverErrors) * time.Second)
			continue
		}

		pause, limited := rateLimitPause(err, p.now())
		if !limited {
			return err
		}
		until := p.now().Add(pause).Local().Format("15:04:05")
		if !p.wait {
			return fmt.Errorf("GitHub's API allowance is used up until %s. Run again then, or add --wait to wait for it: %w", until, err)
		}
		p.notify("GitHub's API allowance is used up. Waiting until %s, then carrying on", until)
		p.sleep(pause)
	}
}

func isServerError(err error) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusInternalServerError
}

// rateLimitPause reads how long GitHub asks a client to wait, in the order
// GitHub's REST documentation gives: Retry-After if present, else the reset time
// when nothing remains, else at least a minute.
func rateLimitPause(err error, now time.Time) (time.Duration, bool) {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) {
		return 0, false
	}
	if httpErr.StatusCode != http.StatusForbidden && httpErr.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}

	if after, err := strconv.Atoi(httpErr.Headers.Get("Retry-After")); err == nil {
		return time.Duration(after) * time.Second, true
	}
	if httpErr.Headers.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(httpErr.Headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// A few seconds past the reset, in case our clock runs ahead of GitHub's.
			return max(time.Unix(reset, 0).Sub(now), 0) + 5*time.Second, true
		}
	}
	if strings.Contains(strings.ToLower(httpErr.Message), "rate limit") {
		return time.Minute, true
	}
	return 0, false
}
