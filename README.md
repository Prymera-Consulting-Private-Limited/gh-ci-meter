# gh ci-meter

Measure a repository's GitHub Actions usage before you pay for more minutes or
set up runners of your own.

One command reads the last 30 days of your workflow runs and tells you:

1. **Minutes and cost.** What your GitHub-hosted minutes cost at list price, and
   what your self-hosted minutes would cost if GitHub's postponed self-hosted
   charge came back.
2. **Waiting against running.** For each job, how long it waited for a runner
   against how long it ran. A job that waits longer than it runs is stuck in a queue.
3. **Busiest hours.** When the work actually arrives. Runners that look idle on
   average can be swamped in bursts.
4. **How many runners.** It replays your real job history on 1, 2, 3… runners of
   your own and finds the smallest pool that keeps nine jobs in ten waiting under
   a minute.
5. **Lane candidates.** Quick jobs, a linter say, that keep waiting behind long
   ones and would stop waiting with a runner of their own.
6. **Runs cancelled before they started.** Runs that a concurrency group quietly
   replaced, leaving commits that were never tested on their own.

## Install

```sh
gh extension install <owner>/gh-ci-meter
```

It runs on the [GitHub CLI](https://cli.github.com) and uses its login, so sign
in first with `gh auth login`. You need read access to the repository's Actions.

## Use

```sh
gh ci-meter                                   # the repository you are in, last 30 days
gh ci-meter acme/widgets --days 14            # another repository, two weeks
gh ci-meter acme/widgets --format markdown > ci-report.md
gh ci-meter acme/widgets --rate linux-8-core=0.022
```

| Flag | Default | What it does |
|---|---|---|
| `--days` | `30` | How many days back to measure. |
| `--format` | `text` | `text`, `markdown` or `json`. |
| `--tz` | your local zone | Time zone for the busiest hours, such as `Europe/London`. |
| `--target-wait` | `1m` | The wait nine jobs in ten should stay under when sizing a pool. |
| `--max-runners` | `10` | The largest pool to try. |
| `--rate LABEL=USD` | | The per-minute price of a runner label the built-in table does not know, such as a larger runner's. Repeat for more. |
| `--no-cache` | off | Read every run's jobs from GitHub again. |
| `--wait` | off | When the API allowance runs out, wait for it to reset and carry on. |

## How it works, and what it assumes

- **What it reads.** Every completed run in the window, then each run's jobs,
  re-runs included: about one API call per run. Busy days are split so that
  GitHub's 1,000-run limit on a listing never cuts any off. A finished run's jobs
  never change, so they are cached (`~/Library/Caches/gh-ci-meter` on macOS,
  `~/.cache/gh-ci-meter` on Linux) and a second report over the same days costs
  almost nothing.
- **The API allowance.** Your `gh` login gets 5,000 API calls an hour, shared by
  every `gh` command and extension you run. A busy repository can run more than a
  thousand workflow runs a day, so a 30-day report can need tens of thousands.
  After listing the runs, and before reading any jobs, the tool counts the calls it
  needs. If they would leave less than a tenth of your allowance, it stops and says
  which `--days` would fit. With `--wait` it reads at GitHub's pace instead,
  waiting as long as GitHub asks whenever the allowance runs out. A passing server
  error from GitHub is retried a few times before the report gives up.
- **Wait** is the time from GitHub queueing a job to a runner picking it up.
  GitHub creates a job only once the jobs it `needs:` have finished, so the wait
  is time spent queueing, not time spent on earlier jobs. Time held for an
  environment approval would count as waiting.
- **Cost** uses GitHub's published per-minute rates, checked on 2026-10-17
  against [Actions runner pricing](https://docs.github.com/en/billing/reference/actions-runner-pricing),
  with each job rounded up to a whole minute as GitHub bills it. Minutes your plan
  includes are not subtracted. Standard runners are
  [free](https://docs.github.com/en/billing/concepts/product-billing/github-actions)
  in a public repository and for Dependabot and GitHub Pages anywhere, and the
  report lists those minutes at no cost; larger runners are always charged.
  Larger runners carry labels their owners choose, so price them with `--rate`.
- **The self-hosted charge.** GitHub
  [announced](https://github.blog/changelog/2025-12-16-coming-soon-simpler-pricing-and-a-better-experience-for-github-actions/)
  a charge of $0.002 a minute for self-hosted runners from 1 March 2026, then
  postponed it. Self-hosted minutes cost nothing today; the report also shows what
  they would cost if it came back.
- **The runner count** replays the jobs in the order they were queued on a pool of
  identical runners, each taking one job at a time. It assumes your runners run each
  job as fast as it ran here, and it ignores concurrency groups, which make some
  runs wait whatever the pool size.
- **A lane candidate** runs in under two minutes but, one time in ten, waits longer
  than it runs. The slowest tenth is used rather than the median because that is when
  a merge is held up.

## Before you run your own runners

A self-hosted runner runs every job as the user it runs as, on the machine it runs
on. Every `npm ci` or `pip install` in a job runs package scripts with that user's
access to files, keys and logins.

- **Private repositories only.** GitHub's advice is that self-hosted runners
  [should almost never be used for public repositories](https://docs.github.com/en/actions/reference/security/secure-use#hardening-for-self-hosted-runners):
  anyone who can open a pull request can run code on them. The report warns when
  the repository is public.
- **Give the runners an OS user of their own**, one that cannot read your home
  directory, or use ephemeral runners that take one job and are removed.
- **Keep two runs from meeting.** Two jobs on two runners of the same machine share
  everything the machine has: a database, a port, a temporary file with a fixed
  name. Give each run its own.

## What a report contains

The repository name, workflow and job names, the default branch's name, counts and
timings. No code, logs or secrets. Self-hosted runners are counted, not named,
because runner names are often machine names. Read the job names before you share
a report in public.

## Licence

[MIT](LICENSE).
