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
	Tag     string       `json:"tag"`
	Build   *runSummary  `json:"build,omitempty"`
	Soaks   []runSummary `json:"soaks,omitempty"`
	Release string       `json:"release,omitempty"`
	Done    bool         `json:"done"`
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

	for {
		result, err := collectWatch(ctx, client, tag)
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

func collectWatch(ctx context.Context, client *gh.Client, tag string) (watchResult, error) {
	result := watchResult{Tag: tag}

	progress, err := client.Progress(ctx, tag)
	if err != nil {
		return result, err
	}

	if progress.Build != nil {
		result.Build = &runSummary{
			Workflow: gh.WorkflowRelease,
			Ref:      progress.Build.HeadBranch,
			State:    progress.Build.State(),
			URL:      progress.Build.URL,
		}
	}

	for _, soak := range progress.Soaks {
		result.Soaks = append(result.Soaks, runSummary{
			Workflow: gh.WorkflowUpgrade,
			Ref:      soak.Event,
			State:    soak.State(),
			URL:      soak.URL,
		})
	}

	release, err := client.Release(ctx, tag)
	if err != nil {
		return result, err
	}

	if release != nil {
		result.Release = release.State()

		// Published is the end of the line. A draft is not: the soak may still
		// be running, and it is the soak that publishes.
		result.Done = !release.Draft
	}

	// A failed build or soak ends it too, and reporting "still going" while
	// nothing is running would wait out the timeout for no reason.
	if progress.Build != nil && progress.Build.Done() && !progress.Build.Succeeded() {
		result.Done = true
	}

	return result, nil
}

// watchVerdict turns the final state into an exit status, so `relctl watch` can
// be the last line of a script and mean something.
//
// The soak's own state is not consulted: it is the soak that publishes, so a
// published release already implies it passed, and a draft already implies it
// did not. Checking both would let them disagree.
func watchVerdict(result watchResult) error {
	if result.Build != nil && result.Build.State != "success" {
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

			rows = append(rows, []string{label, soak.State + "  (" + soak.Ref + ")  " + soak.URL})
		}
	}

	rows = append(rows, []string{"Release:", orNone(result.Release)})

	return table(out, rows)
}
