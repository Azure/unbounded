package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sequenceExecutor struct {
	calls     []string
	results   []error
	callIndex int
}

func useFastWaitPolling(t *testing.T) {
	t.Helper()
	originalDeadline := waitForNodeReadyDeadline
	originalInterval := waitForNodeReadyInterval
	waitForNodeReadyDeadline = 5 * time.Millisecond
	waitForNodeReadyInterval = 0
	t.Cleanup(func() {
		waitForNodeReadyDeadline = originalDeadline
		waitForNodeReadyInterval = originalInterval
	})
}

func (s *sequenceExecutor) Run(_ context.Context, name string, args ...string) error {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, key)
	if s.callIndex >= len(s.results) {
		return nil
	}
	err := s.results[s.callIndex]
	s.callIndex++
	return err
}

func (s *sequenceExecutor) Output(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, key)
	return "", nil
}

func TestBuildRepavePatchIncrementsBothCounters(t *testing.T) {
	patch := BuildRepavePatch(2, 2)
	want := `{"spec":{"operations":{"rebootCounter":3,"repaveCounter":3}}}`
	if patch != want {
		t.Fatalf("patch = %s, want %s", patch, want)
	}
}

func TestBuildRebootPatchIncrementsOnlyReboot(t *testing.T) {
	patch := BuildRebootPatch(4)
	want := `{"spec":{"operations":{"rebootCounter":5}}}`
	if patch != want {
		t.Fatalf("patch = %s, want %s", patch, want)
	}
}

func TestWaitForNodeReadyRetriesUntilReady(t *testing.T) {
	useFastWaitPolling(t)

	exec := &sequenceExecutor{results: []error{fmt.Errorf("not ready yet"), nil}}

	err := WaitForNodeReady(context.Background(), exec, "/root/.kube/config", "stretch-pxe-0")
	if err != nil {
		t.Fatalf("WaitForNodeReady() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s",
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestWaitForNodeStateChangeRequiresObservedTransition(t *testing.T) {
	useFastWaitPolling(t)

	exec := &scriptedExecutor{outputs: map[string][]scriptedOutput{
		"kubectl --kubeconfig /root/.kube/config get node stretch-pxe-0 -o jsonpath={.status.conditions[?(@.type==\"Ready\")].status}": {
			{stdout: "True"},
			{stdout: "True"},
		},
	}}

	err := WaitForNodeStateChange(context.Background(), exec, "/root/.kube/config", "stretch-pxe-0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "did not leave Ready state") {
		t.Fatalf("unexpected error = %v", err)
	}
}

func TestWaitForNodeStateChangeDoesNotTreatCommandErrorsAsTransition(t *testing.T) {
	useFastWaitPolling(t)

	exec := &scriptedExecutor{outputs: map[string][]scriptedOutput{
		"kubectl --kubeconfig /root/.kube/config get node stretch-pxe-0 -o jsonpath={.status.conditions[?(@.type==\"Ready\")].status}": {
			{stdout: "True"},
			{err: fmt.Errorf("temporary kubectl error")},
			{err: fmt.Errorf("temporary kubectl error")},
		},
	}}

	err := WaitForNodeStateChange(context.Background(), exec, "/root/.kube/config", "stretch-pxe-0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "did not leave Ready state") {
		t.Fatalf("unexpected error = %v", err)
	}
	if !strings.Contains(err.Error(), "temporary kubectl error") {
		t.Fatalf("expected last command error to be wrapped, got %v", err)
	}
}
