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

func resolveConfigNodeName(cfg *config.AgentConfig) error {
	nodeName, err := resolveHostNodeName(cfg.NodeName, cfg.MachineName)
	if err != nil {
		return fmt.Errorf("resolve node name: %w", err)
	}

	cfg.NodeName = nodeName
	return nil
}

func resolveHostNodeName(configNodeName, machineName string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	return resolveNodeName(configNodeName, hostname, machineName)
}

func resolveNodeName(configNodeName, hostname, machineName string) (string, error) {
	if nodeName := strings.TrimSpace(configNodeName); nodeName != "" {
		if isValidKubernetesNodeName(nodeName) {
			return nodeName, nil
		}

		return "", fmt.Errorf("node name override %q is not a valid Kubernetes node name", nodeName)
	}

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
