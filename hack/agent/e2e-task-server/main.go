// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// e2e-task-server is a minimal gRPC server that implements the TaskServer
// service for end-to-end testing. It sends up to two NodeUpdateSpec tasks:
//
//  1. A "no-drift" task with the current cluster Kubernetes version.
//  2. An optional "upgrade" task with a different version (triggers
//     drift detection and a blue-green update attempt).
//
// Usage:
//
//	go run ./hack/agent/e2e-task-server --listen=:50051 --kubernetes-version=1.33.1
//	go run ./hack/agent/e2e-task-server --listen=:50051 --kubernetes-version=1.33.1 --upgrade-kubernetes-version=1.33.2
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
)

func main() {
	listen := flag.String("listen", ":50051", "gRPC listen address")
	kubeVersion := flag.String("kubernetes-version", "", "Kubernetes version for the no-drift task (required)")
	upgradeVersion := flag.String("upgrade-kubernetes-version", "", "Kubernetes version for the upgrade task (optional)")
	flag.Parse()

	if *kubeVersion == "" {
		log.Fatal("--kubernetes-version is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}

	tasks := []*agentv1.Task{
		{
			Id: "e2e-update-001",
			Spec: &agentv1.Task_NodeUpdate{
				NodeUpdate: &agentv1.NodeUpdateSpec{
					KubernetesVersion: *kubeVersion,
				},
			},
			CreatedAt: timestamppb.Now(),
		},
	}

	if *upgradeVersion != "" {
		tasks = append(tasks, &agentv1.Task{
			Id: "e2e-upgrade-002",
			Spec: &agentv1.Task_NodeUpdate{
				NodeUpdate: &agentv1.NodeUpdateSpec{
					KubernetesVersion: *upgradeVersion,
				},
			},
			CreatedAt: timestamppb.Now(),
		})
		log.Printf("upgrade task configured: version %s -> %s", *kubeVersion, *upgradeVersion)
	}

	srv := &server{
		tasks:   tasks,
		reports: make(map[string]*reportEntry),
	}

	gs := grpc.NewServer()
	agentv1.RegisterTaskServerServer(gs, srv)

	go func() {
		<-ctx.Done()
		log.Println("shutting down gRPC server")
		gs.GracefulStop()
	}()

	log.Printf("e2e-task-server listening on %s (tasks: %d)", lis.Addr(), len(tasks))
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// reportEntry holds the status report and a channel that is closed when
// the report is received.
type reportEntry struct {
	req  *agentv1.ReportTaskStatusRequest
	done chan struct{} // closed when the report arrives
}

// server implements agentv1.TaskServerServer for e2e testing.
type server struct {
	agentv1.UnimplementedTaskServerServer

	tasks []*agentv1.Task

	mu      sync.Mutex
	reports map[string]*reportEntry // keyed by task ID
}

// getOrCreateEntry returns the report entry for the given task ID,
// creating one if it does not exist. Must be called with s.mu held.
func (s *server) getOrCreateEntry(taskID string) *reportEntry {
	e, ok := s.reports[taskID]
	if !ok {
		e = &reportEntry{done: make(chan struct{})}
		s.reports[taskID] = e
	}
	return e
}

// waitForReport blocks until the report for taskID arrives or the context
// is cancelled.
func (s *server) waitForReport(ctx context.Context, taskID string) error {
	s.mu.Lock()
	e := s.getOrCreateEntry(taskID)
	s.mu.Unlock()

	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PullTasks sends tasks to the agent one at a time. For each task after
// the first, it waits for the previous task's status report before sending
// the next one. The stream is held open after all tasks are sent.
func (s *server) PullTasks(_ *agentv1.PullTasksRequest, stream grpc.ServerStreamingServer[agentv1.PullTasksResponse]) error {
	for i, task := range s.tasks {
		// Wait for the previous task to be reported before sending the next.
		if i > 0 {
			prevID := s.tasks[i-1].GetId()
			log.Printf("PullTasks: waiting for report on task %s before sending next", prevID)
			if err := s.waitForReport(stream.Context(), prevID); err != nil {
				return fmt.Errorf("wait for task %s report: %w", prevID, err)
			}
		}

		log.Printf("PullTasks: sending task %d/%d: %s", i+1, len(s.tasks), task.GetId())
		if err := stream.Send(&agentv1.PullTasksResponse{Task: task}); err != nil {
			return fmt.Errorf("send task %s: %w", task.GetId(), err)
		}
	}

	log.Printf("PullTasks: all %d tasks sent, holding stream open", len(s.tasks))

	// Hold the stream open until the client disconnects or the server
	// shuts down.
	<-stream.Context().Done()
	return stream.Context().Err()
}

// ReportTaskStatus records the agent's status report and logs it.
func (s *server) ReportTaskStatus(_ context.Context, req *agentv1.ReportTaskStatusRequest) (*agentv1.ReportTaskStatusResponse, error) {
	log.Printf("ReportTaskStatus: task_id=%s state=%s message=%q",
		req.GetTaskId(), req.GetState(), req.GetMessage())

	s.mu.Lock()
	e := s.getOrCreateEntry(req.GetTaskId())
	if e.req == nil {
		e.req = req
		close(e.done)
	}
	s.mu.Unlock()

	return &agentv1.ReportTaskStatusResponse{}, nil
}

// WaitForReport blocks until a status report for the given task ID is
// received or the timeout expires.
func (s *server) WaitForReport(taskID string, timeout time.Duration) (*agentv1.ReportTaskStatusRequest, error) {
	s.mu.Lock()
	e := s.getOrCreateEntry(taskID)
	s.mu.Unlock()

	select {
	case <-e.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return e.req, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for status report on %s after %s", taskID, timeout)
	}
}
