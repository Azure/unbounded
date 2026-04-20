// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Operation constants shared with the agent daemon's opshim layer.
// When migrating to a dedicated CRD, these will move into the API package.
const (
	softRebootNamespace = "unbounded-system"
	softRebootLabelKey  = "unbounded.io/agent-op"
	softRebootDataKey   = "operations"
)

type softRebootOp struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

func machineSoftRebootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "soft-reboot NAME",
		Short: "Soft-reboot an agent-managed machine (restarts nspawn container in place)",
		Long: `Soft-reboot restarts the nspawn machine on the target node without
reprovisioning the rootfs. The kubelet and containerd services are
stopped, the nspawn container is restarted, and services are brought
back up.

This command creates an operation ConfigMap that the agent daemon
watches. The agent processes the operation and updates the ConfigMap
state to "completed" or "failed".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runSoftReboot(ctx, c, args[0])
		},
	}

	return cmd
}

func runSoftReboot(ctx context.Context, c client.WithWatch, name string) error {
	// Verify the Machine CR exists before creating the operation.
	if _, err := getMachine(ctx, c, name); err != nil {
		return err
	}

	// Build the operation ConfigMap.
	ops := []softRebootOp{{Type: "reboot", State: "pending"}}

	opsJSON, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshalling operations: %w", err)
	}

	cmName := fmt.Sprintf("op-%s-%d", name, time.Now().Unix())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: softRebootNamespace,
			Labels: map[string]string{
				softRebootLabelKey: name,
			},
		},
		Data: map[string]string{
			softRebootDataKey: string(opsJSON),
		},
	}

	if err := c.Create(ctx, cm); err != nil {
		return fmt.Errorf("creating operation ConfigMap: %w", err)
	}

	printStep(fmt.Sprintf("Soft-rebooting Machine %s...", name))
	printConfig("configmap", cmName)
	printConfig("namespace", softRebootNamespace)
	fmt.Println()

	return watchSoftReboot(ctx, c, cmName)
}

// watchSoftReboot watches the operation ConfigMap until all operations reach
// a terminal state (completed or failed).
func watchSoftReboot(ctx context.Context, c client.WithWatch, cmName string) error {
	watcher, err := c.Watch(ctx, &corev1.ConfigMapList{},
		client.InNamespace(softRebootNamespace),
		client.MatchingFields{"metadata.name": cmName},
	)
	if err != nil {
		return fmt.Errorf("watching ConfigMap: %w", err)
	}
	defer watcher.Stop()

	for ev := range watcher.ResultChan() {
		if ev.Type == watch.Error {
			return fmt.Errorf("watch error: %v", ev.Object)
		}

		if ev.Type == watch.Deleted {
			return fmt.Errorf("operation ConfigMap %s was deleted", cmName)
		}

		cm, ok := ev.Object.(*corev1.ConfigMap)
		if !ok {
			continue
		}

		ops, err := parseSoftRebootOps(cm)
		if err != nil {
			continue
		}

		// Print state transitions.
		for i := range ops {
			switch ops[i].State {
			case "in_progress":
				printStep(fmt.Sprintf("Operation %d: %s in progress...", i, ops[i].Type))
			case "completed":
				printStep(fmt.Sprintf("Operation %d: %s completed", i, ops[i].Type))
			case "failed":
				printStep(fmt.Sprintf("Operation %d: %s failed: %s", i, ops[i].Type, ops[i].Message))
			}
		}

		// Check if all operations are terminal.
		if allTerminal(ops) {
			if anyFailed(ops) {
				return fmt.Errorf("one or more operations failed")
			}

			printReady()

			return nil
		}
	}

	return fmt.Errorf("watch closed before soft-reboot completed")
}

func parseSoftRebootOps(cm *corev1.ConfigMap) ([]softRebootOp, error) {
	raw, ok := cm.Data[softRebootDataKey]
	if !ok {
		return nil, fmt.Errorf("missing %s key", softRebootDataKey)
	}

	var ops []softRebootOp
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		return nil, err
	}

	return ops, nil
}

func allTerminal(ops []softRebootOp) bool {
	for i := range ops {
		if ops[i].State != "completed" && ops[i].State != "failed" {
			return false
		}
	}

	return len(ops) > 0
}

func anyFailed(ops []softRebootOp) bool {
	for i := range ops {
		if ops[i].State == "failed" {
			return true
		}
	}

	return false
}
