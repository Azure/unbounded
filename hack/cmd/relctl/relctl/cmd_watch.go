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
commit.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(cmd.Context(), cmd.OutOrStdout(), opts, args[0], interval, timeout, once)
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 20*time.Second, "How often to poll")
	cmd.Flags().DurationVar(&timeout, "timeout", 90*time.Minute, "Give up after this long")
	cmd.Flags().BoolVar(&once, "once", false, "Report current state and exit rather than polling")

	return cmd
}

func runWatch(
	ctx context.Context,
	out io.Writer,
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

	// Correlation candidates are fetched once for the whole watch, not once per
	// poll. Who cut a tag cannot change while we watch it, and a 90 minute
	// watch at the default interval is over 250 polls: refetching would add
	// that many requests to answer a question already answered.
	prepares, err := client.Prepares(ctx)
	if err != nil {
		return err
	}

	for {
		result, err := collectWatch(ctx, client, tag, prepares)
		if err != nil {
			return err
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

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func collectWatch(ctx context.Context, client *gh.Client, tag string, prepares []gh.Run) (watchResult, error) {
	result := watchResult{Tag: tag}

	progress, err := client.Progress(ctx, tag)
	if err != nil {
		return result, err
	}

	if progress.Build != nil {
		attribution := gh.Attribute(*progress.Build, prepares)
		result.By = &attribution

		result.Build = &runSummary{
			Workflow:  gh.WorkflowRelease,
			Ref:       progress.Build.HeadBranch,
			Event:     progress.Build.Event,
			State:     progress.Build.State(),
			Succeeded: progress.Build.Succeeded(),
			URL:       progress.Build.URL,
			Actor:     progress.Build.Actor,
			By:        &attribution,
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

	if label, value := attributionRow(result.By); label != "" {
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
func attributionRow(attribution *gh.Attribution) (label, value string) {
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
	default:
		return "Cut by:", "unknown  (no release-prepare run covers this tag push)"
	}
}
