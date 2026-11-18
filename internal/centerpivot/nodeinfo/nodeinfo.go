// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nodeinfo publishes a node's agent-info annotation from the host's
// files, until trident-acl-agent writes it itself (designs/centerpivot.md,
// section 4.3). It runs in the centerpivot-node-info DaemonSet.
package nodeinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded/internal/centerpivot/identity"
)

// Publisher keeps one Node's agent-info annotation current.
type Publisher struct {
	Client   kubernetes.Interface
	NodeName string
	// MachineIDPath is the host's /etc/machine-id, which Unbounded's host
	// agent binds read-only into the node's nspawn machine.
	MachineIDPath string
	// OSReleasePath is the host's os-release, which systemd-nspawn binds
	// into every machine.
	OSReleasePath string
	// VersionKey is the os-release key the agent reads its version from.
	VersionKey string
	// Annotation is the agent-info annotation's key.
	Annotation string
	Log        logr.Logger
}

// Read returns the agent-info for the host's files: the instance ID the agent
// sends Nebraska, and the version it reports.
func (p *Publisher) Read() (identity.AgentInfo, error) {
	machineID, err := os.ReadFile(p.MachineIDPath)
	if err != nil {
		return identity.AgentInfo{}, fmt.Errorf("read the host's machine ID: %w", err)
	}

	osRelease, err := os.ReadFile(p.OSReleasePath)
	if err != nil {
		return identity.AgentInfo{}, fmt.Errorf("read the host's os-release: %w", err)
	}

	version, _ := identity.OSReleaseValue(string(osRelease), p.VersionKey)

	return identity.NewAgentInfo(string(machineID), version)
}

// Publish writes the agent-info to the Node when it changed.
func (p *Publisher) Publish(ctx context.Context) error {
	info, err := p.Read()
	if err != nil {
		return err
	}

	value, err := info.Marshal()
	if err != nil {
		return err
	}

	node, err := p.Client.CoreV1().Nodes().Get(ctx, p.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Node %s: %w", p.NodeName, err)
	}

	if node.Annotations[p.Annotation] == value {
		return nil
	}

	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{p.Annotation: value}}})
	if err != nil {
		return fmt.Errorf("encode agent-info patch: %w", err)
	}

	if _, err := p.Client.CoreV1().Nodes().Patch(ctx, p.NodeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch Node %s: %w", p.NodeName, err)
	}

	p.Log.Info("Published agent-info", "node", p.NodeName, "machineId", info.MachineID, "currentVersion", info.CurrentVersion)

	return nil
}

// Run publishes at once, then every interval, until ctx ends.
func (p *Publisher) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := p.Publish(ctx); err != nil {
			p.Log.Error(err, "Publishing agent-info failed; trying again", "node", p.NodeName, "in", interval)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
