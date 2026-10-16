// gh ci-meter measures a repository's GitHub Actions usage before you decide
// whether to run your own runners: what the minutes cost, how long jobs wait for
// a runner against how long they run, when the bursts come, how many runners of
// your own would keep waits short, which quick jobs deserve a lane, and which
// runs GitHub cancelled before they started.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

const usage = `Measure a repository's GitHub Actions usage.

Usage:
  gh ci-meter [OWNER/REPO] [flags]

With no repository, the one in the current directory is measured.

Flags:
`

type options struct {
	Repo       string
	Days       int
	Format     string
	Location   *time.Location
	TargetWait time.Duration
	MaxRunners int
	Rates      rateFlag
	NoCache    bool
	Wait       bool
}

// rateFlag collects --rate LABEL=USD, the per-minute price of a runner label
// the built-in table does not know, such as a larger runner's.
type rateFlag map[string]float64

func (r rateFlag) String() string {
	var pairs []string
	for label, usd := range r {
		pairs = append(pairs, fmt.Sprintf("%s=%v", label, usd))
	}
	return strings.Join(pairs, ",")
}

func (r rateFlag) Set(value string) error {
	label, price, found := strings.Cut(value, "=")
	if !found || label == "" {
		return errors.New("want LABEL=USD, for example linux-8-core=0.022")
	}
	usd, err := strconv.ParseFloat(price, 64)
	if err != nil || usd < 0 {
		return fmt.Errorf("%q is not a price in dollars a minute", price)
	}
	r[label] = usd
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gh ci-meter: %s\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	repo, err := resolveRepo(opts.Repo)
	if err != nil {
		return err
	}
	client, err := api.DefaultRESTClient()
	if err != nil {
		return fmt.Errorf("cannot reach GitHub through gh (run `gh auth login` first): %w", err)
	}

	progress := func(format string, args ...any) {
		fmt.Fprintf(stderr, "\r"+format+"\033[K", args...)
	}
	f := &fetcher{
		client:   &pacedClient{client: client, wait: opts.Wait, notify: progress, sleep: time.Sleep, now: time.Now},
		repo:     repo,
		cacheDir: cacheDir(opts.NoCache),
		progress: progress,
	}

	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-time.Duration(opts.Days) * 24 * time.Hour)

	info, err := f.repoInfo()
	if err != nil {
		return err
	}
	runs, err := f.completedRuns(from, to)
	if err != nil {
		return err
	}
	if err := checkAllowance(f, runs, opts, stderr); err != nil {
		fmt.Fprint(stderr, "\r\033[K")
		return err
	}
	withJobs, err := f.jobsOf(runs)
	fmt.Fprint(stderr, "\r\033[K")
	if err != nil {
		return fmt.Errorf("%w (jobs already read are cached; run again to carry on)", err)
	}

	report := analyze(info, withJobs, from, to, analysisOptions{
		Location:   opts.Location,
		TargetWait: opts.TargetWait,
		MaxRunners: opts.MaxRunners,
		Rates:      opts.Rates,
	})
	return writeReport(stdout, report, opts.Format)
}

// checkAllowance stops a report that would use up the API allowance before it
// reads a single job, unless --wait says to sit the limit out. GitHub Enterprise
// Server can run without rate limits, and then has no rate_limit endpoint to
// ask, so a failed lookup skips the check rather than the report.
func checkAllowance(f *fetcher, runs []workflowRun, opts options, stderr io.Writer) error {
	left, err := f.allowance()
	if err != nil {
		return nil
	}
	plan := planFetch(f.uncachedRuns(runs), opts.Days, left)
	if plan.Fits {
		return nil
	}
	if !opts.Wait {
		return errors.New(plan.refusal(left))
	}
	fmt.Fprintf(stderr, "\r%s API calls to make and %s left until %s: this will wait for GitHub about %d time(s).\033[K\n",
		formatInt(plan.Calls), formatInt(left.Remaining), left.Reset.Local().Format("15:04"), max(plan.WaitResets, 1))
	return nil
}

// parseOptions accepts the repository before or after the flags, as gh's own
// commands do.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{Rates: rateFlag{}}
	var zone string

	flags := flag.NewFlagSet("ci-meter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, usage)
		flags.PrintDefaults()
	}
	flags.IntVar(&opts.Days, "days", 30, "how many days back to measure")
	flags.StringVar(&opts.Format, "format", "text", "text, markdown or json")
	flags.StringVar(&zone, "tz", "Local", "time zone for the busiest hours, such as Europe/London")
	flags.DurationVar(&opts.TargetWait, "target-wait", time.Minute, "longest wait 9 jobs in 10 should see when sizing a pool")
	flags.IntVar(&opts.MaxRunners, "max-runners", 10, "largest pool to try when sizing")
	flags.Var(opts.Rates, "rate", "per-minute price of a runner label, as LABEL=USD; repeat for more")
	flags.BoolVar(&opts.NoCache, "no-cache", false, "read every run's jobs from GitHub again")
	flags.BoolVar(&opts.Wait, "wait", false, "when the API allowance runs out, wait for it to reset and carry on")

	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if rest := flags.Args(); len(rest) > 0 {
		opts.Repo = rest[0]
		if err := flags.Parse(rest[1:]); err != nil {
			return opts, err
		}
		if len(flags.Args()) > 0 {
			return opts, fmt.Errorf("one repository at a time, got %q too", flags.Args()[0])
		}
	}

	switch {
	case opts.Days < 1:
		return opts, errors.New("--days must be at least 1")
	case opts.MaxRunners < 1:
		return opts, errors.New("--max-runners must be at least 1")
	case opts.TargetWait < 0:
		return opts, errors.New("--target-wait cannot be negative")
	case opts.Format != "text" && opts.Format != "markdown" && opts.Format != "json":
		return opts, fmt.Errorf("--format must be text, markdown or json, not %q", opts.Format)
	}

	loc, err := time.LoadLocation(zone)
	if err != nil {
		return opts, fmt.Errorf("unknown time zone %q", zone)
	}
	opts.Location = loc
	return opts, nil
}

func resolveRepo(arg string) (string, error) {
	if arg == "" {
		current, err := repository.Current()
		if err != nil {
			return "", errors.New("not in a GitHub repository; name one as OWNER/REPO")
		}
		return fmt.Sprintf("%s/%s", current.Owner, current.Name), nil
	}
	parsed, err := repository.Parse(arg)
	if err != nil {
		return "", fmt.Errorf("%q is not OWNER/REPO", arg)
	}
	return fmt.Sprintf("%s/%s", parsed.Owner, parsed.Name), nil
}

func cacheDir(disabled bool) string {
	if disabled {
		return ""
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "gh-ci-meter")
}
