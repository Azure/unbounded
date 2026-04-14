// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
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
			defer conn.Close()

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

				// Stub: sleep 30s then report success.
				log.Info("executing task (stub: sleeping 30s)", "task_id", task.GetId())

				select {
				case <-time.After(30 * time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}

				_, err = client.ReportTaskStatus(ctx, &agentv1.ReportTaskStatusRequest{
					TaskId:  task.GetId(),
					State:   agentv1.TaskState_TASK_STATE_SUCCEEDED,
					Message: "task completed (stub)",
				})
				if err != nil {
					return fmt.Errorf("report task status for %q: %w", task.GetId(), err)
				}

				log.Info("reported task success", "task_id", task.GetId())
			}
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "gRPC endpoint of the task server (required)")

	return cmd
}
