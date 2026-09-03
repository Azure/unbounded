// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// watchResult is where a release has got to.
type watchResult struct {
	Tag   string      `json:"tag"`
	Build *runSummary `json:"build,omitempty"`
	// By is who cut this release, which is not who GitHub says.
	//
	// Carried on the result rather than on Build because it is a fact about the
	// TAG: the soak inherits the same distorted actor, so repeating it per run
	// would say the same thing three times and imply three observations.
	By      *gh.Attribution `json:"by,omitempty"`
	Soaks   []runSummary    `json:"soaks,omitempty"`
	Release string          `json:"release,omitempty"`
	Done    bool            `json:"done"`
	// AttributionError is set when the correlation candidates could not be
	// listed, so an unknown cutter cannot be misread as an established fact
	// about the tag. Reported rather than fatal: it costs a derived column,
	// not the state this command exists to report.
	AttributionError string `json:"attributionError,omitempty"`
}

func watchCommand(opts *Options) *cobra.Command {
	var (
		interval time.Duration
		timeout  time.Duration
		once     bool
	)

	cmd := &cobra.Command{
		Use:   "watch <tag>",
		Short: "Follow a release through build, soak and publish",
		Long: `Follow a release from its tag to a published release.

Three workflows are involved and none of them names the tag except the first:
release.yaml is identifiable because a tag push sets the run's branch to the
tag, while release-upgrade fires on workflow_run and reports the default
branch. Its runs are matched by commit and time window, which is what keeps a
candidate's soak apart from its final's when a promote puts both on the same
commit.

A poll that fails for a reason that could pass later - a 5xx, a rate limit, a
connection that never landed - is retried until the timeout, and reported on
stderr so it cannot corrupt --output json. Anything GitHub answered definitely,
such as a 404 or a bad credential, fails immediately rather than spending the
timeout on an answer that will not change. --once never retries.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(
				cmd.Context(),
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
				opts, args[0], interval, timeout, once,
			)
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 20*time.Second, "How often to poll")
	cmd.Flags().DurationVar(&timeout, "timeout", 90*time.Minute, "Give up after this long")
	cmd.Flags().BoolVar(&once, "once", false, "Report current state and exit rather than polling")

	return cmd
}

func runWatch(
	ctx context.Context,
	out, errOut io.Writer,
	opts *Options,
	tag string,
	interval, timeout time.Duration,
	once bool,
) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	if opts.Output == OutputGitHub {
		return fmt.Errorf("watch has no github output; use json")
	}

	client, err := opts.client(ctx)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	prepares := &prepareCache{client: client}

	for {
		result, err := collectWatch(ctx, client, tag, prepares)
		if err != nil {
			// A watch is a ninety minute proposition, and until now any single
			// bad response ended it. A registry of a release nobody is
			// touching should not be abandoned because one of roughly a
			// thousand requests came back 502.
			//
			// --once is exempt. It is a single-shot query documented to report
			// current state and exit, so a caller who asked one question gets
			// one answer or an error, never a wait.
			if once || !gh.Transient(err) || !time.Now().Before(deadline) {
				return watchFailure(tag, timeout, deadline, err)
			}

			// To stderr, never out: out carries the JSON that -o json callers
			// parse, and progress noise in it would corrupt the document.
			//
			// Unchecked, unlike every other write in this file. This is an
			// advisory note about a failure already being handled, on the
			// diagnostic stream rather than the product one. Failing the watch
			// because the note could not be written would turn a blip we just
			// recovered from into the outage.
			//
			//nolint:errcheck // advisory progress note; see above
			fmt.Fprintf(errOut, "relctl: %v; retrying in %s\n", err, interval)

			if err := wait(ctx, interval); err != nil {
				return err
			}

			continue
		}

		if opts.Output == OutputJSON {
			if err := writeJSON(out, result); err != nil {
				return err
			}
		} else if err := writeWatchText(out, result); err != nil {
			return err
		}

		if once || result.Done {
			if !result.Done && once {
				return nil
			}

			return watchVerdict(result)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up waiting for %s after %s", tag, timeout)
		}

		if err := wait(ctx, interval); err != nil {
			return err
		}
	}
}

// wait sleeps for the poll interval, or stops early if the caller gives up.
func wait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// watchFailure explains why a watch stopped.
//
// The deadline case names the error rather than only the timeout, because
// "gave up waiting for v0.5.0 after 1h30m0s" is what a watch says when nothing
// happened, and a watch that spent ninety minutes being told 502 should not be
// indistinguishable from one that spent them waiting.
func watchFailure(tag string, timeout time.Duration, deadline time.Time, err error) error {
	if !time.Now().Before(deadline) {
		return fmt.Errorf("gave up waiting for %s after %s: %w", tag, timeout, err)
	}

	return err
}

// prepareCache holds the correlation candidates for the length of a watch.
//
// Fetched on the first sighting of a BUILD, not before the loop. `watch <tag>`
// is routinely started before the tag exists - the documented flow in
// RELEASING.md is `relctl cut` and then `relctl watch <tag>` - and a list taken
// then cannot contain the prepare run that is about to push the tag. Attribute
// reads that absence as "pushed by hand" and reports the run's actor, which on
// a tag push is the deploy key's owner. Waiting for the build removes the race
// entirely: the prepare pushed the tag, so it necessarily precedes the run the
// push created.
//
// Fetched once rather than per poll, because who cut a tag cannot change while
// we watch it and a 90 minute watch at the default interval is over 250 polls.
//
// A failure is not cached. Attribution is a cosmetic column on a command whose
// job is to follow a release for an hour and a half, so a bad minute must not
// decide the answer for the rest of it.
type prepareCache struct {
	client *gh.Client
	got    bool
	value  []gh.Run
}

// get returns the candidates, fetching them the first time it succeeds.
//
// A failure reports nil candidates and the error, and leaves the cache empty so
// the next poll tries again. Nil is a safe list to attribute against: no
// candidate contains the build and none covers it either, so every run reports
// unknown rather than a guess.
func (p *prepareCache) get(ctx context.Context) ([]gh.Run, error) {
	if p.got {
		return p.value, nil
	}

	value, err := p.client.Prepares(ctx)
	if err != nil {
		return nil, err
	}

	p.got, p.value = true, value

	return value, nil
}

func collectWatch(
	ctx context.Context,
	client *gh.Client,
	tag string,
	prepares *prepareCache,
) (watchResult, error) {
	result := watchResult{Tag: tag}

	progress, err := client.Progress(ctx, tag)
	if err != nil {
		return result, err
	}

	if progress.Build != nil {
		// Fetched here rather than up front, so the list is taken at a moment
		// when it can contain the prepare that pushed this tag. See
		// prepareCache.
		//
		// A failure is reported in the rendering and does not fail the poll.
		// The build, the soaks and the release all arrived; refetching them to
		// recover one derived field would trade real state for a cosmetic one.
		candidates, err := prepares.get(ctx)
		if err != nil {
			result.AttributionError = err.Error()
		}

		attribution := gh.Attribute(*progress.Build, candidates)
		result.By = &attribution

		result.Build = &runSummary{
			Workflow:  gh.WorkflowRelease,
			Ref:       progress.Build.HeadBranch,
			Event:     progress.Build.Event,
			State:     progress.Build.State(),
			Succeeded: progress.Build.Succeeded(),
			URL:       progress.Build.URL,
			// Actor, but deliberately not By. The derived answer is a fact
			// about the TAG and is carried once on the result; repeating it
			// here put it in -o json twice and implied two observations of
			// something established once.
			Actor: progress.Build.Actor,
		}
	}

	for _, soak := range progress.Soaks {
		result.Soaks = append(result.Soaks, runSummary{
			Workflow:  gh.WorkflowUpgrade,
			Ref:       soak.HeadBranch,
			Event:     soak.Event,
			State:     soak.State(),
			Succeeded: soak.Succeeded(),
			URL:       soak.URL,
			Actor:     soak.Actor,
		})
	}

	release, err := client.Release(ctx, tag)
	if err != nil {
		return result, err
	}

	var current *gh.Release

	if release != nil {
		result.Release = release.State()
		current = release
	}

	result.Done = watchDone(progress.Build, progress.Soaks, current)

	return result, nil
}

// watchDone decides whether anything further will happen.
//
// Pure, and separated from the fetching, because the terminating conditions are
// the part worth testing and getting them wrong costs a 90 minute timeout
// rather than a wrong answer.
//
// Publication alone is not enough. It is the SOAK that publishes, so a failed
// soak leaves the release a draft forever: reading Done from !Draft would poll
// every 20 seconds until the timeout with nothing running.
func watchDone(build *gh.Run, soaks []gh.Run, release *gh.Release) bool {
	// A failed build never produces a release at all.
	if build != nil && build.Done() && !build.Succeeded() {
		return true
	}

	// Only the NEWEST soak decides. An earlier failure followed by a running
	// retry is a recovery in progress, not an ending.
	if len(soaks) > 0 {
		newest := soaks[len(soaks)-1] // Progress returns them oldest first.
		if newest.Done() && !newest.Succeeded() {
			return true
		}
	}

	return release != nil && !release.Draft
}

// watchVerdict turns the final state into an exit status, so `relctl watch` can
// be the last line of a script and mean something.
//
// The soak's own state is not consulted: it is the soak that publishes, so a
// published release already implies it passed, and a draft already implies it
// did not. Checking both would let them disagree.
func watchVerdict(result watchResult) error {
	if result.Build != nil && !result.Build.Succeeded {
		return fmt.Errorf("build for %s is %s", result.Tag, result.Build.State)
	}

	switch result.Release {
	case "":
		return fmt.Errorf("no release exists for %s", result.Tag)
	case "draft":
		return fmt.Errorf("%s is still a draft; the soak has not published it", result.Tag)
	}

	return nil
}

func writeWatchText(out io.Writer, result watchResult) error {
	rows := [][]string{{"Tag:", result.Tag}}

	if label, value := attributionRow(result.By, result.AttributionError); label != "" {
		rows = append(rows, []string{label, value})
	}

	if result.Build == nil {
		rows = append(rows, []string{"Build:", "not started"})
	} else {
		rows = append(rows, []string{"Build:", result.Build.State + "  " + result.Build.URL})
	}

	switch len(result.Soaks) {
	case 0:
		rows = append(rows, []string{"Soak:", "not started"})
	default:
		for i, soak := range result.Soaks {
			label := "Soak:"
			if i > 0 {
				label = "  retry:"
			}

			rows = append(rows, []string{label, soak.State + "  (" + soak.Event + ")  " + soak.URL})
		}
	}

	rows = append(rows, []string{"Release:", orNone(result.Release)})

	return table(out, rows)
}

// attributionRow renders who is behind a tag, or nothing when there is no build
// to attribute yet.
//
// The label distinguishes the two ways a tag arrives, because they are
// different facts and the second is worth noticing: a tag that release-prepare
// did not push skipped every guard that workflow applies.
//
// An unknown attribution still prints a row. The alternative is silence, which
// reads as "nobody", and the whole reason this exists is that the obvious
// reading of the actor is wrong.
//
// The two unknowns are told apart. "No prepare covers this" is a finding about
// the tag; "we could not fetch the candidates" is a finding about the network,
// and reporting the first when the second happened would invite someone to
// conclude a tag skipped release-prepare when nothing of the sort was
// established.
func attributionRow(attribution *gh.Attribution, failure string) (label, value string) {
	if attribution == nil {
		return "", ""
	}

	switch {
	case attribution.Source == gh.SourceDispatch && attribution.Known():
		value = attribution.By
		if attribution.RunURL != "" {
			value += "  (release-prepare " + attribution.RunURL + ")"
		}

		return "Cut by:", value
	case attribution.Source == gh.SourcePush && attribution.Known():
		return "Pushed by:", attribution.By + "  (tag pushed by hand, not by release-prepare)"
	case failure != "":
		return "Cut by:", "unknown  (could not list release-prepare runs: " + failure + ")"
	default:
		return "Cut by:", "unknown  (no release-prepare run covers this tag push)"
	}
}
