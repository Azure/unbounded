// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// NodeIdentity identifies the nspawn machine, Kubernetes Machine CR, and
// Kubernetes Node that represent a resolved agent machine goal state.
type NodeIdentity struct {
	// MachineName is the local systemd-nspawn machine name (e.g. "kube1").
	MachineName string

	// KubeMachineName is the Kubernetes Machine CR name (e.g. "agent-e2e").
	KubeMachineName string

	// NodeName is the Kubernetes Node name used by kubelet and host-side daemon watches.
	NodeName string
}

// ResolveNodeIdentity resolves the agent machine/node identity shared by host
// daemon code and nspawn machine goal state.
func ResolveNodeIdentity(cfg *config.AgentConfig, machineName string) (*NodeIdentity, error) {
	nodeName, err := ResolveHostNodeName(cfg.MachineName)
	if err != nil {
		return nil, fmt.Errorf("resolve node name: %w", err)
	}

	return &NodeIdentity{
		MachineName:     machineName,
		KubeMachineName: cfg.MachineName,
		NodeName:        nodeName,
	}, nil
}

// ResolveHostNodeName resolves the Kubernetes Node name for this host.
func ResolveHostNodeName(machineName string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	return ResolveNodeName(hostname, machineName)
}

// ResolveNodeName chooses the Kubernetes Node name from the host name when it
// is valid, falling back to the Machine CR name.
func ResolveNodeName(hostname, machineName string) (string, error) {
	if nodeName := strings.TrimSpace(hostname); isValidKubernetesNodeName(nodeName) {
		return nodeName, nil
	}

	nodeName := strings.TrimSpace(machineName)
	if isValidKubernetesNodeName(nodeName) {
		return nodeName, nil
	}

	return "", fmt.Errorf("machine name %q is not a valid Kubernetes node name", machineName)
}

func isValidKubernetesNodeName(name string) bool {
	if name == "" {
		return false
	}

	return len(validation.IsDNS1123Subdomain(name)) == 0
}
