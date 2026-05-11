// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

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
