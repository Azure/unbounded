// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
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

			return runSoftReboot(ctx, c, args[0], ttl)
		},
	}

	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")

	return cmd
}

func runSoftReboot(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32) error {
	// Fetch the Machine CR to get its UID for the owner reference.
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	// Build the MachineOperation CR.
	opName := fmt.Sprintf("%s-reboot-%d", name, time.Now().Unix())

	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: opName,
			Labels: map[string]string{
				v1alpha3.MachineOperationMachineLabelKey: name,
			},
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
			OperationName: v1alpha3.OperationReboot,
		},
	}

	if ttlSeconds > 0 {
		op.Spec.TTLSecondsAfterFinished = &ttlSeconds
	}

	if err := c.Create(ctx, op); err != nil {
		return fmt.Errorf("creating MachineOperation: %w", err)
	}

	printStep(fmt.Sprintf("Soft-rebooting Machine %s...", name))
	printConfig("operation", opName)
	fmt.Println()

	return watchMachineOperation(ctx, c, opName)
}

// watchMachineOperation watches a MachineOperation CR until it reaches a
// terminal phase (Complete or Failed).
func watchMachineOperation(ctx context.Context, c client.WithWatch, opName string) error {
	watcher, err := c.Watch(ctx, &v1alpha3.MachineOperationList{},
		client.MatchingFields{"metadata.name": opName},
	)
	if err != nil {
		return fmt.Errorf("watching MachineOperation: %w", err)
	}
	defer watcher.Stop()

	var lastPhase v1alpha3.OperationPhase

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
		if phase != lastPhase {
			switch phase {
			case v1alpha3.OperationPhaseInProgress:
				printStep(fmt.Sprintf("Operation %s: %s in progress...", op.Spec.OperationName, opName))
			case v1alpha3.OperationPhaseComplete:
				printStep(fmt.Sprintf("Operation %s: %s completed", op.Spec.OperationName, opName))
			case v1alpha3.OperationPhaseFailed:
				printStep(fmt.Sprintf("Operation %s: %s failed: %s", op.Spec.OperationName, opName, op.Status.Message))
			}

			lastPhase = phase
		}

		if op.Status.IsTerminal() {
			if phase == v1alpha3.OperationPhaseFailed {
				return fmt.Errorf("operation failed: %s", op.Status.Message)
			}

			printReady()

			return nil
		}
	}

	return fmt.Errorf("watch closed before operation completed")
}
