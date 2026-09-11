// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// targetReady requires Ready status belonging to the running target. Freshness
// is checked separately through its Lease: Node conditions may report slowly.
// Both callers read through live API connections, not informer caches.
func targetReady(node *corev1.Node, name, bootID string) error {
	if bootID == "" || node.Name != name || node.UID == "" || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != bootID {
		return fmt.Errorf("node identity does not match the running repave target")
	}

	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			if condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
	}

	return fmt.Errorf("target Node is not Ready")
}

// A same-named Lease from a previous Node incarnation cannot vouch for this
// target. Honor the kubelet's advertised duration without assuming its Node
// status-report interval or manufacturing freshness from an API read.
func targetLeaseFresh(lease *coordinationv1.Lease, node *corev1.Node, now time.Time) error {
	if lease.Name != node.Name || lease.Namespace != corev1.NamespaceNodeLease || lease.DeletionTimestamp != nil ||
		lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != node.Name {
		return fmt.Errorf("lease identity does not match target Node")
	}

	owned := false

	for _, owner := range lease.OwnerReferences {
		if owner.APIVersion == "v1" && owner.Kind == "Node" && owner.Name == node.Name && owner.UID == node.UID && node.UID != "" {
			owned = true
		}
	}

	if !owned {
		return fmt.Errorf("lease does not belong to target Node UID")
	}

	if lease.Spec.RenewTime == nil || lease.Spec.RenewTime.IsZero() || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return fmt.Errorf("target Node lease lacks a valid renewal or duration")
	}

	renewed := lease.Spec.RenewTime.Time
	if renewed.After(now.Add(30*time.Second)) || !now.Before(renewed.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second)) {
		return fmt.Errorf("target Node lease is expired or its renewal is in the future")
	}

	return nil
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
	output, err := run(ctx, s.Target, filepath.Join("/", goalstates.BinDir, "kubectl"), "--kubeconfig="+goalstates.KubeletKubeconfigPath,
		"--request-timeout=10s", "get", "node", s.TargetConfig.NodeName, "-o", "json")
	if err != nil {
		return fmt.Errorf("target kubelet credentials have not established API access: %w", err)
	}

	var authenticated corev1.Node
	if err := json.Unmarshal([]byte(output), &authenticated); err != nil {
		return fmt.Errorf("decode target Node observation: %w", err)
	}

	if err := targetReady(&authenticated, s.TargetConfig.NodeName, boot); err != nil {
		return err
	}

	var observed corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: s.TargetConfig.NodeName}, &observed); err != nil {
		return err
	}

	if err := targetReady(&observed, s.TargetConfig.NodeName, boot); err != nil {
		return err
	}

	if observed.UID != authenticated.UID {
		return fmt.Errorf("target Node changed during verification")
	}

	var lease coordinationv1.Lease
	if err := reader.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: observed.Name}, &lease); err != nil {
		return fmt.Errorf("read target Node lease: %w", err)
	}

	if err := targetLeaseFresh(&lease, &observed, time.Now()); err != nil {
		return err
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
