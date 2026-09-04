// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type nodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

type daemonSetStatus struct {
	Status struct {
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		UpdatedNumberScheduled int `json:"updatedNumberScheduled"`
		NumberReady            int `json:"numberReady"`
		NumberAvailable        int `json:"numberAvailable"`
		NumberUnavailable      int `json:"numberUnavailable"`
	} `json:"status"`
}

func (b *benchmark) validateContext(ctx context.Context) error {
	output, err := b.commands.Run(ctx, nil, "kubectl", "config", "current-context")
	if err != nil {
		return err
	}

	current := strings.TrimSpace(string(output))
	if b.config.ConfirmedContext == "" {
		return fmt.Errorf("set BENCHMARK_CONFIRM_CONTEXT=%q to confirm the target cluster", current)
	}

	if b.config.ConfirmedContext != current {
		return fmt.Errorf("confirmed BENCHMARK_CONFIRM_CONTEXT=%q does not match current context %q", b.config.ConfirmedContext, current)
	}

	return nil
}

func (b *benchmark) targetNodes(ctx context.Context) ([]string, error) {
	platformParts := strings.SplitN(b.config.ImagePlatform, "/", 2)
	if len(platformParts) != 2 || platformParts[0] == "" || platformParts[1] == "" {
		return nil, fmt.Errorf("image platform BENCHMARK_IMAGE_PLATFORM=%q must have os/architecture form", b.config.ImagePlatform)
	}

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}

	var list nodeList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode node list: %w", err)
	}

	result := make([]string, 0, len(list.Items))
	for _, node := range list.Items {
		if node.Metadata.Labels["kubernetes.io/os"] != platformParts[0] ||
			node.Metadata.Labels["kubernetes.io/arch"] != platformParts[1] ||
			node.Spec.Unschedulable {
			continue
		}

		ready := false

		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true

				break
			}
		}

		if ready {
			result = append(result, node.Metadata.Name)
		}
	}

	if len(result) != b.config.NodeCount {
		return nil, fmt.Errorf(
			"found %d schedulable Ready %s nodes, want exactly %d",
			len(result),
			b.config.ImagePlatform,
			b.config.NodeCount,
		)
	}

	return result, nil
}

func (b *benchmark) validateGantry(ctx context.Context) error {
	daemonSet, err := b.gantryDaemonSetStatus(ctx)
	if err != nil {
		return err
	}

	return validateGantryStatus(daemonSet, b.config.NodeCount)
}

func (b *benchmark) validateGantryAtCurrentSize(ctx context.Context) error {
	daemonSet, err := b.gantryDaemonSetStatus(ctx)
	if err != nil {
		return err
	}

	if daemonSet.Status.DesiredNumberScheduled <= 0 {
		return fmt.Errorf("gantry DaemonSet has no desired pods")
	}

	return validateGantryStatus(daemonSet, daemonSet.Status.DesiredNumberScheduled)
}

func (b *benchmark) gantryDaemonSetStatus(ctx context.Context) (daemonSetStatus, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"get", "daemonset", b.config.GantryDaemonSet,
		"-o", "json",
	)
	if err != nil {
		return daemonSetStatus{}, err
	}

	var daemonSet daemonSetStatus
	if err := json.Unmarshal(output, &daemonSet); err != nil {
		return daemonSetStatus{}, fmt.Errorf("decode gantry DaemonSet: %w", err)
	}

	return daemonSet, nil
}

func validateGantryStatus(daemonSet daemonSetStatus, expectedCount int) error {
	status := daemonSet.Status
	if status.DesiredNumberScheduled != expectedCount ||
		status.UpdatedNumberScheduled != expectedCount ||
		status.NumberReady != expectedCount ||
		status.NumberAvailable != expectedCount ||
		status.NumberUnavailable != 0 {
		return fmt.Errorf(
			"gantry DaemonSet is not converged: desired=%d updated=%d ready=%d available=%d unavailable=%d, want %d ready",
			status.DesiredNumberScheduled,
			status.UpdatedNumberScheduled,
			status.NumberReady,
			status.NumberAvailable,
			status.NumberUnavailable,
			expectedCount,
		)
	}

	return nil
}
