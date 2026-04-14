// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/nodeupdate"
	"github.com/Azure/unbounded-kube/internal/version"
)

// Retry parameters for the daemon task-pull loop.
const (
	// initialBackoff is the delay before the first reconnect attempt.
	initialBackoff = 1 * time.Second
	// maxBackoff caps the exponential backoff between reconnect attempts.
	maxBackoff = 60 * time.Second
	// backoffMultiplier scales the delay after each consecutive failure.
	backoffMultiplier = 2.0

	// reportRetries is the number of times to retry ReportTaskStatus on
	// transient errors before giving up.
	reportRetries = 5
	// reportRetryDelay is the base delay between ReportTaskStatus retries.
	reportRetryDelay = 2 * time.Second
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	var endpoint string

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the agent daemon to pull and execute tasks",
		Long: `Run a long-lived daemon that connects to the task server via gRPC,
pulls tasks as they arrive, executes them, and reports the result back.

The daemon automatically reconnects with exponential backoff when the
server disconnects or becomes temporarily unavailable.`,
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
				grpc.WithKeepaliveParams(keepalive.ClientParameters{
					// Send keepalive pings every 30s to detect dead connections.
					Time: 30 * time.Second,
					// Wait 10s for a ping ack before considering the connection dead.
					Timeout: 10 * time.Second,
					// Send pings even when there are no active RPCs, since the
					// daemon is idle between tasks.
					PermitWithoutStream: true,
				}),
			)
			if err != nil {
				return fmt.Errorf("create gRPC client for %q: %w", endpoint, err)
			}
			defer conn.Close() //nolint:errcheck // Best-effort close of gRPC connection.

			client := agentv1.NewTaskServerClient(conn)

			return runPullLoop(ctx, log, client)
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "gRPC endpoint of the task server (required)")

	return cmd
}

// runPullLoop opens a PullTasks stream and processes tasks. When the stream
// breaks (server restart, network blip, EOF) it reconnects with exponential
// backoff. It only returns when the context is cancelled.
func runPullLoop(ctx context.Context, log *slog.Logger, client agentv1.TaskServerClient) error {
	backoff := initialBackoff

	for {
		err := pullAndProcess(ctx, log, client)
		if ctx.Err() != nil {
			// Context cancelled (e.g. SIGINT) - exit cleanly.
			log.Info("daemon stopping", "reason", ctx.Err())
			return nil
		}

		log.Warn("task-pull stream disconnected, will reconnect",
			"error", err,
			"backoff", backoff,
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		// Exponential backoff capped at maxBackoff.
		backoff = time.Duration(math.Min(
			float64(backoff)*backoffMultiplier,
			float64(maxBackoff),
		))
	}
}

// pullAndProcess opens a single PullTasks stream and processes tasks until
// the stream ends or an error occurs. On a successful stream open the backoff
// is reset via the returned error (nil means clean EOF from server).
func pullAndProcess(ctx context.Context, log *slog.Logger, client agentv1.TaskServerClient) error {
	stream, err := client.PullTasks(ctx, &agentv1.PullTasksRequest{})
	if err != nil {
		return fmt.Errorf("open PullTasks stream: %w", err)
	}

	log.Info("task-pull stream opened, waiting for tasks")

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			log.Info("task-pull stream closed by server")
			return fmt.Errorf("stream closed by server (EOF)")
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

		if err := reportStatusWithRetry(ctx, log, client, task.GetId(), state, message); err != nil {
			// Log the failure but keep the stream alive. The server
			// can resend the task if it never received the report.
			log.Error("failed to report task status after retries",
				"task_id", task.GetId(),
				"error", err,
			)
		}

		log.Info("reported task status",
			"task_id", task.GetId(),
			"state", state,
		)
	}
}

// reportStatusWithRetry calls ReportTaskStatus with retries on transient
// errors. It gives up after reportRetries attempts.
func reportStatusWithRetry(
	ctx context.Context,
	log *slog.Logger,
	client agentv1.TaskServerClient,
	taskID string,
	state agentv1.TaskState,
	message string,
) error {
	var lastErr error

	for attempt := range reportRetries {
		_, err := client.ReportTaskStatus(ctx, &agentv1.ReportTaskStatusRequest{
			TaskId:  taskID,
			State:   state,
			Message: message,
		})
		if err == nil {
			return nil
		}

		lastErr = err
		delay := reportRetryDelay * time.Duration(1<<attempt)
		if delay > maxBackoff {
			delay = maxBackoff
		}

		log.Warn("ReportTaskStatus failed, retrying",
			"task_id", taskID,
			"attempt", attempt+1,
			"error", err,
			"retry_in", delay,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("ReportTaskStatus failed after %d attempts: %w", reportRetries, lastErr)
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
// the applied config and performing an nspawn machine update if
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

	log.Info("drift detected, starting node update")

	// Merge the spec into the current config.
	newCfg := nodeupdate.MergeSpec(active.Config, spec)

	// Execute the node update.
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
