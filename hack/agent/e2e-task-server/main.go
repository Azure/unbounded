// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// e2e-task-server is a minimal gRPC server that implements the TaskServer
// service for end-to-end testing. It queues a single NodeUpdateSpec task
// (Kubernetes upgrade to 1.33.4) and logs the status report from the agent.
//
// Usage:
//
//	go run ./hack/agent/e2e-task-server --listen=:50051
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
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}

	srv := &server{
		task: &agentv1.Task{
			Id: "e2e-upgrade-001",
			Spec: &agentv1.Task_NodeUpdate{
				NodeUpdate: &agentv1.NodeUpdateSpec{
					KubernetesVersion: "1.33.4",
				},
			},
			CreatedAt: timestamppb.Now(),
		},
		done: make(chan struct{}),
	}

	gs := grpc.NewServer()
	agentv1.RegisterTaskServerServer(gs, srv)

	go func() {
		<-ctx.Done()
		log.Println("shutting down gRPC server")
		gs.GracefulStop()
	}()

	log.Printf("e2e-task-server listening on %s", lis.Addr())
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// server implements agentv1.TaskServerServer for e2e testing.
type server struct {
	agentv1.UnimplementedTaskServerServer

	task *agentv1.Task

	// mu protects reported.
	mu       sync.Mutex
	reported *agentv1.ReportTaskStatusRequest

	// done is closed when a status report is received.
	done chan struct{}
}

// PullTasks sends the pre-configured task to the agent and then keeps the
// stream open until the client disconnects or the server shuts down.
func (s *server) PullTasks(_ *agentv1.PullTasksRequest, stream grpc.ServerStreamingServer[agentv1.PullTasksResponse]) error {
	log.Printf("PullTasks: agent connected, sending task %s", s.task.GetId())

	if err := stream.Send(&agentv1.PullTasksResponse{Task: s.task}); err != nil {
		return fmt.Errorf("send task: %w", err)
	}

	log.Printf("PullTasks: task %s sent, holding stream open", s.task.GetId())

	// Hold the stream open until the client disconnects or the context
	// is cancelled (server shutdown).
	<-stream.Context().Done()
	return stream.Context().Err()
}

// ReportTaskStatus records the agent's status report and logs it.
func (s *server) ReportTaskStatus(_ context.Context, req *agentv1.ReportTaskStatusRequest) (*agentv1.ReportTaskStatusResponse, error) {
	log.Printf("ReportTaskStatus: task_id=%s state=%s message=%q",
		req.GetTaskId(), req.GetState(), req.GetMessage())

	s.mu.Lock()
	first := s.reported == nil
	s.reported = req
	s.mu.Unlock()

	if first {
		close(s.done)
	}

	return &agentv1.ReportTaskStatusResponse{}, nil
}

// WaitForReport blocks until a status report is received or the timeout
// expires. This is intended for programmatic callers (not used by gRPC
// clients).
func (s *server) WaitForReport(timeout time.Duration) (*agentv1.ReportTaskStatusRequest, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.reported, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for status report after %s", timeout)
	}
}
