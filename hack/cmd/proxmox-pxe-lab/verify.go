package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	waitForNodeReadyDeadline = 15 * time.Minute
	waitForNodeReadyInterval = 10 * time.Second
)

func BuildRepavePatch(rebootCounter, repaveCounter int) string {
	return fmt.Sprintf(`{"spec":{"operations":{"rebootCounter":%d,"repaveCounter":%d}}}`, rebootCounter+1, repaveCounter+1)
}

func BuildRebootPatch(rebootCounter int) string {
	return fmt.Sprintf(`{"spec":{"operations":{"rebootCounter":%d}}}`, rebootCounter+1)
}

type MachineOperationCounters struct {
	Reboot int
	Repave int
}

func ReadMachineOperationCounters(ctx context.Context, exec Executor, kubeconfig, machineName string) (MachineOperationCounters, error) {
	rebootCounter, err := readMachineOperationCounter(ctx, exec, kubeconfig, machineName, "rebootCounter")
	if err != nil {
		return MachineOperationCounters{}, err
	}
	repaveCounter, err := readMachineOperationCounter(ctx, exec, kubeconfig, machineName, "repaveCounter")
	if err != nil {
		return MachineOperationCounters{}, err
	}
	return MachineOperationCounters{Reboot: rebootCounter, Repave: repaveCounter}, nil
}

func readMachineOperationCounter(ctx context.Context, exec Executor, kubeconfig, machineName, field string) (int, error) {
	out, err := exec.Output(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "machine", machineName, "-o", fmt.Sprintf("jsonpath={.spec.operations.%s}", field))
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse %s for machine %s: %w", field, machineName, err)
	}
	return value, nil
}

func WaitForNodeStateChange(ctx context.Context, exec Executor, kubeconfig, nodeName string) error {
	deadline := time.Now().Add(waitForNodeReadyDeadline)
	var lastErr error
	for {
		status, err := exec.Output(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "node", nodeName, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		if err == nil && strings.TrimSpace(status) != "True" {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("node %s did not leave Ready state before timeout: %w", nodeName, lastErr)
			}
			return fmt.Errorf("node %s did not leave Ready state before timeout", nodeName)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if waitForNodeReadyInterval > 0 {
			select {
			case <-time.After(waitForNodeReadyInterval):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func WaitForNodeReady(ctx context.Context, exec Executor, kubeconfig, nodeName string) error {
	deadline := time.Now().Add(waitForNodeReadyDeadline)
	for {
		err := exec.Run(ctx, "kubectl", "--kubeconfig", kubeconfig, "wait", "--for=condition=Ready", "node/"+nodeName, "--timeout=30s")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not become Ready before timeout: %w", nodeName, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if waitForNodeReadyInterval > 0 {
			select {
			case <-time.After(waitForNodeReadyInterval):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
