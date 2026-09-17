// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newControllerRootCommand builds controller operation commands.
func newControllerRootCommand(rt *pluginRuntime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "controller",
		Aliases: []string{"controllers"},
		Short:   "Controller operations",
	}
	cmd.AddCommand(newControllerLogsCommand(rt))
	cmd.AddCommand(newControllerProxyCommand(rt, false))
	cmd.AddCommand(newControllerStatusJSONCommand(rt))

	return cmd
}

// newControllerLogsCommand builds controller logs command.
func newControllerLogsCommand(rt *pluginRuntime) *cobra.Command {
	var (
		selector   string
		container  string
		follow     bool
		previous   bool
		tail       int64
		since      time.Duration
		sinceTime  string
		timestamps bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show logs from the unbounded controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := rt.namespace()
			if err != nil {
				return err
			}

			client, err := rt.kubeClient()
			if err != nil {
				return err
			}

			pods, err := client.CoreV1().Pods(ns).List(cmd.Context(), v1.ListOptions{LabelSelector: selector})
			if err != nil {
				return err
			}

			if len(pods.Items) == 0 {
				return fmt.Errorf("no pods matched selector %q in namespace %q", selector, ns)
			}

			sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
			pod := pods.Items[0]

			logOpts := &corev1.PodLogOptions{
				Container:  container,
				Follow:     follow,
				Previous:   previous,
				Timestamps: timestamps,
			}
			if cmd.Flags().Changed("tail") {
				logOpts.TailLines = &tail
			}

			if cmd.Flags().Changed("since") {
				logOpts.SinceSeconds = ptrInt64(int64(since.Seconds()))
			}

			if cmd.Flags().Changed("since-time") {
				parsed, parseErr := time.Parse(time.RFC3339, sinceTime)
				if parseErr != nil {
					return fmt.Errorf("invalid --since-time %q: %w", sinceTime, parseErr)
				}

				logOpts.SinceTime = &v1.Time{Time: parsed}
			}

			req := client.CoreV1().Pods(ns).GetLogs(pod.Name, logOpts)

			stream, err := req.Stream(cmd.Context())
			if err != nil {
				return err
			}

			defer func() {
				_ = stream.Close() //nolint:errcheck
			}()

			_, err = io.Copy(cmd.OutOrStdout(), stream)

			return err
		},
	}
	cmd.Flags().StringVarP(&selector, "selector", "l", defaultControllerSelector, "Label selector for controller pods")
	cmd.Flags().StringVar(&container, "container", "", "Print logs of this container")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Specify if the logs should be streamed")
	cmd.Flags().BoolVar(&previous, "previous", false, "Print logs for the previous instance of the container in a pod")
	cmd.Flags().Int64Var(&tail, "tail", -1, "Lines of recent log file to display")
	cmd.Flags().DurationVar(&since, "since", 0, "Only return logs newer than a relative duration like 5s, 2m, or 3h")
	cmd.Flags().StringVar(&sinceTime, "since-time", "", "Only return logs after a specific RFC3339 timestamp")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "Include timestamps on each line in the log output")

	return cmd
}

// newControllerStatusJSONCommand exports the controller's cluster overview.
func newControllerStatusJSONCommand(rt *pluginRuntime) *cobra.Command {
	var pretty bool

	fetch := defaultNodeStatusFetchOptions()

	cmd := &cobra.Command{
		Use:   "status-json",
		Short: "Dump summary JSON from the controller",
		Long: "Export /status/json, preserving controller metadata and summary fields. " +
			"Legacy full-node responses are projected into summaries without diagnostic arrays.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := nodeStatusFetchFromCommand(cmd).merged(fetch)

			raw, err := fetchClusterStatusRaw(rt, cmd, opts)
			if err != nil {
				return err
			}

			status, err := clusterSummaryJSON(raw)
			if err != nil {
				return err
			}

			var data []byte
			if pretty {
				data, err = json.MarshalIndent(status, "", "  ")
			} else {
				data, err = json.Marshal(status)
			}

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)

			return err
		},
	}
	addNodeStatusFetchFlags(cmd.Flags(), fetch)
	cmd.Flags().BoolVar(&pretty, "pretty", true, "Pretty-print JSON output")

	return cmd
}

// clusterSummaryJSON preserves unknown fields while removing legacy node details.
func clusterSummaryJSON(raw []byte) (json.RawMessage, error) {
	summary, err := decodeClusterSummary(raw)
	if err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}

	if _, legacy := fields["nodes"]; !legacy {
		return raw, nil
	}

	delete(fields, "nodes")

	if _, current := fields["nodeSummaries"]; !current {
		fields["nodeSummaries"], err = json.Marshal(summary.NodeSummaries)
		if err != nil {
			return nil, err
		}

		fields["nodeCount"], err = json.Marshal(summary.NodeCount)
		if err != nil {
			return nil, err
		}
	}

	return json.Marshal(fields)
}
