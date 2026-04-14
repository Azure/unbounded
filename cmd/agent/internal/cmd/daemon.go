// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/nodeupdate"
	"github.com/Azure/unbounded-kube/internal/version"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	var endpoint string

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the agent daemon to pull and execute tasks",
		Long: `Run a long-lived daemon that connects to the task server via gRPC,
pulls tasks as they arrive, executes them, and reports the result back.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()
			log := cmdCtx.Logger

			if endpoint == "" {
				return fmt.Errorf("--endpoint is required")
			}

			log.Info("starting daemon",
				"version", version.Version,
				"commit", version.GitCommit,
				"endpoint", endpoint,
			)

			conn, err := grpc.NewClient(
				endpoint,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				return fmt.Errorf("dial task server %q: %w", endpoint, err)
			}
			defer func() { _ = conn.Close() }()

			client := agentv1.NewTaskServerClient(conn)

			stream, err := client.PullTasks(ctx, &agentv1.PullTasksRequest{})
			if err != nil {
				return fmt.Errorf("open PullTasks stream: %w", err)
			}

			log.Info("task-pull stream opened, waiting for tasks")

			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					log.Info("task-pull stream closed by server")
					return nil
				}

				if err != nil {
					return fmt.Errorf("receive task: %w", err)
				}

				task := resp.GetTask()
				if task == nil {
					log.Warn("received empty task, skipping")
					continue
				}

				log.Info("received task", "task_id", task.GetId())

				state, message := executeTask(ctx, log, task)

				_, err = client.ReportTaskStatus(ctx, &agentv1.ReportTaskStatusRequest{
					TaskId:  task.GetId(),
					State:   state,
					Message: message,
				})
				if err != nil {
					return fmt.Errorf("report task status for %q: %w", task.GetId(), err)
				}

				log.Info("reported task status",
					"task_id", task.GetId(),
					"state", state,
				)
			}
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "gRPC endpoint of the task server (required)")

	return cmd
}

// executeTask dispatches a task to the appropriate handler and returns the
// result state and message.
func executeTask(ctx context.Context, log *slog.Logger, task *agentv1.Task) (agentv1.TaskState, string) {
	switch spec := task.GetSpec().(type) {
	case *agentv1.Task_NodeUpdate:
		return executeNodeUpdate(ctx, log, spec.NodeUpdate)
	default:
		log.Warn("unknown task type, skipping", "task_id", task.GetId())
		return agentv1.TaskState_TASK_STATE_FAILED, "unknown task type"
	}
}

// executeNodeUpdate handles a NodeUpdateSpec task by detecting drift against
// the applied config and performing a blue-green nspawn machine update if
// needed.
func executeNodeUpdate(ctx context.Context, log *slog.Logger, spec *agentv1.NodeUpdateSpec) (agentv1.TaskState, string) {
	log.Info("executing node update task",
		"kubernetes_version", spec.GetKubernetesVersion(),
	)

	// Find the currently active machine and its applied config.
	active, err := nodeupdate.FindActiveMachine()
	if err != nil {
		log.Error("failed to find active machine", "error", err)
		return agentv1.TaskState_TASK_STATE_FAILED, fmt.Sprintf("find active machine: %v", err)
	}

	log.Info("found active machine",
		"machine", active.Name,
		"current_version", active.Config.Cluster.Version,
	)

	// Check for drift.
	if !nodeupdate.HasDrift(active.Config, spec) {
		log.Info("no drift detected, nothing to do")
		return agentv1.TaskState_TASK_STATE_SUCCEEDED, "no drift detected"
	}

	log.Info("drift detected, starting blue-green update")

	// Merge the spec into the current config.
	newCfg := nodeupdate.MergeSpec(active.Config, spec)

	// Execute the blue-green update.
	if err := nodeupdate.Execute(ctx, log, active, newCfg); err != nil {
		log.Error("node update failed", "error", err)
		return agentv1.TaskState_TASK_STATE_FAILED, fmt.Sprintf("node update: %v", err)
	}

	return agentv1.TaskState_TASK_STATE_SUCCEEDED, fmt.Sprintf(
		"node updated: version %s, machine %s -> %s",
		newCfg.Cluster.Version,
		active.Name,
		goalstates.AlternateMachine(active.Name),
	)
}
