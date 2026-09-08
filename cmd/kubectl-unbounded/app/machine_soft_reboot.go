// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// defaultTTLSeconds is the default TTL for completed/failed MachineOperation CRs.
const defaultTTLSeconds = 300 // 5 minutes

func machineSoftRebootCommand() *cobra.Command {
	var ttl int32

	cmd := &cobra.Command{
		Use:   "soft-reboot NAME",
		Short: "Soft-reboot an agent-managed machine (restarts nspawn container in place)",
		Long: `Soft-reboot restarts the nspawn machine on the target node without
reprovisioning the rootfs. The kubelet and containerd services are
stopped, the nspawn container is restarted, and services are brought
back up.

This command creates a MachineOperation CR that the agent daemon watches.
The agent processes the operation and updates the MachineOperation status
to "Complete" or "Failed".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runSoftReboot(ctx, c, args[0], ttl, cmd.OutOrStdout())
		},
	}

	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")

	return cmd
}

func runSoftReboot(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32, out io.Writer) error {
	opName := fmt.Sprintf("%s-reboot-%d", name, time.Now().Unix())

	if err := createMachineOperation(ctx, c, name, opName, v1alpha3.OperationNodeReboot, ttlSeconds); err != nil {
		return err
	}

	printStep(out, fmt.Sprintf("Soft-rebooting Machine %s...", name))
	printConfig(out, "operation", opName)
	fprintln(out)

	return watchMachineOperation(ctx, c, opName, out)
}

func createMachineOperation(ctx context.Context, c client.WithWatch, name, opName string, kind v1alpha3.OperationKind, ttlSeconds int32) error {
	// Fetch the Machine CR to get its UID for the owner reference.
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: opName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: v1alpha3.GroupVersion.String(),
					Kind:       "Machine",
					Name:       machine.Name,
					UID:        machine.UID,
				},
			},
		},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    name,
			OperationKind: kind,
		},
	}

	if ttlSeconds > 0 {
		op.Spec.TTLSecondsAfterFinished = &ttlSeconds
	}

	if err := c.Create(ctx, op); err != nil {
		return fmt.Errorf("creating MachineOperation: %w", err)
	}

	return nil
}

// watchMachineOperation watches a MachineOperation CR until it reaches a
// terminal phase (Complete or Failed).
func watchMachineOperation(ctx context.Context, c client.WithWatch, opName string, out io.Writer) error {
	var initial v1alpha3.MachineOperation
	if err := c.Get(ctx, client.ObjectKey{Name: opName}, &initial); err != nil {
		return fmt.Errorf("getting MachineOperation: %w", err)
	}

	if initial.Status.IsTerminal() {
		return finishMachineOperationWait(&initial, out)
	}

	return watchMachineOperationFromResourceVersion(ctx, c, opName, initial.ResourceVersion, out)
}

func watchMachineOperationFromResourceVersion(ctx context.Context, c client.WithWatch, opName, resourceVersion string, out io.Writer) error {
	listOptions := []client.ListOption{
		client.MatchingFields{"metadata.name": opName},
	}
	if resourceVersion != "" {
		listOptions = append(listOptions, &client.ListOptions{
			Raw: &metav1.ListOptions{
				ResourceVersion: resourceVersion,
			},
		})
	}

	watcher, err := c.Watch(
		ctx, &v1alpha3.MachineOperationList{},
		listOptions...,
	)
	if err != nil {
		return fmt.Errorf("watching MachineOperation: %w", err)
	}
	defer watcher.Stop()

	var lastPhase v1alpha3.OperationPhase

	seenConditions := map[string]conditionState{}
	seenTargets := map[string]string{}

	for ev := range watcher.ResultChan() {
		if ev.Type == watch.Error {
			return fmt.Errorf("watch error: %v", ev.Object)
		}

		if ev.Type == watch.Deleted {
			return fmt.Errorf("operation %s was deleted", opName)
		}

		op, ok := ev.Object.(*v1alpha3.MachineOperation)
		if !ok {
			continue
		}

		phase := op.Status.Phase
		reportConditionTransitions(out, op.Status.Conditions, seenConditions)
		reportTargetTransitions(out, op.Status.Targets, seenTargets)

		if phase != lastPhase {
			switch phase {
			case v1alpha3.OperationPhaseInProgress:
				printStep(out, fmt.Sprintf("Operation %s: %s in progress...", op.Spec.OperationKind, opName))
			case v1alpha3.OperationPhaseComplete:
				printStep(out, fmt.Sprintf("Operation %s: %s completed", op.Spec.OperationKind, opName))
			case v1alpha3.OperationPhaseFailed:
				printStep(out, fmt.Sprintf("Operation %s: %s failed: %s", op.Spec.OperationKind, opName, op.Status.Message))
			}

			lastPhase = phase
		}

		if op.Status.IsTerminal() {
			if phase == v1alpha3.OperationPhaseFailed {
				return fmt.Errorf("operation failed: %s", op.Status.Message)
			}

			printReady(out)

			return nil
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return fmt.Errorf("watch closed before operation completed")
}

func reportTargetTransitions(out io.Writer, targets []v1alpha3.MachineOperationTargetStatus, seen map[string]string) {
	for _, target := range targets {
		key := target.MachineRef
		if key == "" {
			continue
		}

		dedup := string(target.Phase) + "/" + string(target.Stage)
		if seen[key] == dedup {
			continue
		}

		seen[key] = dedup

		text := stageText(target.Stage)
		if text == "" {
			text = targetTransitionState(target)
		}

		printStep(out, fmt.Sprintf("Target %s: %s", key, text))
	}
}

func stageText(stage v1alpha3.OperationStage) string {
	switch stage {
	case v1alpha3.OperationStagePoweringOff, v1alpha3.OperationStageWaitingOff:
		return "Powering off host"
	case v1alpha3.OperationStagePoweringOn, v1alpha3.OperationStageWaitingOn:
		return "Powering on host"
	case v1alpha3.OperationStageRepaveRequested, v1alpha3.OperationStageWaitingRepave:
		return "Booting PXE installer"
	case v1alpha3.OperationStageWaitingCloudInit:
		return "Running first-boot cloud-init"
	case v1alpha3.OperationStageWaitingNode:
		return "Waiting for node to join cluster"
	default:
		return ""
	}
}

func targetTransitionState(target v1alpha3.MachineOperationTargetStatus) string {
	parts := make([]string, 0, 3)

	if target.Phase != "" {
		parts = append(parts, string(target.Phase))
	}

	if target.Stage != "" {
		parts = append(parts, string(target.Stage))
	}

	state := strings.Join(parts, "/")
	if state == "" {
		state = "Pending"
	}

	if target.Message == "" {
		return state
	}

	return fmt.Sprintf("%s - %s", state, target.Message)
}
