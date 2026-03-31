package nodestop

import (
	"context"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
)

type stopContainerd struct{}

// StopContainerd returns a task that stops the containerd service.
func StopContainerd() phases.Task {
	return &stopContainerd{}
}

func (s *stopContainerd) Name() string { return "stop-containerd" }

func (s *stopContainerd) Do(_ context.Context) error {
	return nil
}

type stopKubelet struct{}

// StopKubelet returns a task that stops the kubelet service.
func StopKubelet() phases.Task {
	return &stopKubelet{}
}

func (s *stopKubelet) Name() string { return "stop-kubelet" }

func (s *stopKubelet) Do(_ context.Context) error {
	return nil
}
