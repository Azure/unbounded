// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// targetReady accepts only a fresh Ready observation for the running target.
// Both callers read through live API connections, not informer caches.
func targetReady(node *corev1.Node, name, bootID string, now time.Time) error {
	if bootID == "" || node.Name != name || node.UID == "" || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != bootID {
		return fmt.Errorf("node identity does not match the running repave target")
	}

	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			age := now.Sub(condition.LastHeartbeatTime.Time)
			if condition.Status == corev1.ConditionTrue && !condition.LastHeartbeatTime.IsZero() && age >= -30*time.Second && age <= 2*time.Minute {
				return nil
			}
		}
	}

	return fmt.Errorf("target Node has no fresh Ready status")
}

func verifyRepaveTarget(ctx context.Context, log *slog.Logger, reader client.Reader, s *repaveState) error {
	return verifyRepaveTargetWithRunner(ctx, reader, s, func(ctx context.Context, machine string, args ...string) (string, error) {
		return executil.MachineRun(ctx, log, machine, args...)
	})
}

func verifyRepaveTargetWithRunner(ctx context.Context, reader client.Reader, s *repaveState, run func(context.Context, string, ...string) (string, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	boot, err := run(ctx, s.Target, "cat", "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return fmt.Errorf("observe target boot: %w", err)
	}

	boot = strings.TrimSpace(boot)
	// Run inside the target so nspawn-relative certificate/exec-plugin paths
	// retain their meaning. This must authenticate with the real kubelet config,
	// not the daemon controller certificate or the bootstrap token config.
	output, err := run(ctx, s.Target, "/usr/local/bin/kubectl", "--kubeconfig="+goalstates.KubeletKubeconfigPath,
		"--request-timeout=10s", "get", "node", s.TargetConfig.NodeName, "-o", "json")
	if err != nil {
		return fmt.Errorf("target kubelet credentials have not established API access: %w", err)
	}

	var authenticated corev1.Node
	if err := json.Unmarshal([]byte(output), &authenticated); err != nil {
		return fmt.Errorf("decode target Node observation: %w", err)
	}

	if err := targetReady(&authenticated, s.TargetConfig.NodeName, boot, time.Now()); err != nil {
		return err
	}

	var observed corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: s.TargetConfig.NodeName}, &observed); err != nil {
		return err
	}

	if err := targetReady(&observed, s.TargetConfig.NodeName, boot, time.Now()); err != nil {
		return err
	}

	if observed.UID != authenticated.UID {
		return fmt.Errorf("target Node changed during verification")
	}
	// Recheck the running target after both API reads to reject a restart during
	// observation. Persisted evidence authorizes source deletion, not liveness forever.
	after, err := run(ctx, s.Target, "cat", "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}

	if strings.TrimSpace(after) != boot {
		return fmt.Errorf("target restarted during verification")
	}

	now := metav1.Now().Time
	s.TargetBootID, s.TargetNodeUID, s.VerifiedAt = boot, string(observed.UID), &now
	s.Phase = "committed"

	return nil
}
