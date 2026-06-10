// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package config defines the intermediate representation (IR) for agent
// configuration. These types are the shared contract between the agent
// library and its consumers.
//
// Consumers should define their own public-facing configuration
// representation and translate to/from these IR types. For example, AKS
// Flex Node maintains its own JSON config schema and maps it to these
// structs before calling agent library functions.
//
// TODO: the versioning and stability guarantees for this package are not
// yet finalized and will be revisited in a future iteration.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// AgentConfig is the core configuration needed to bootstrap a Kubernetes
// node. It contains only the cloud-agnostic fields required by the shared
// agent library. Platform-specific extensions (e.g. attestation, cloud
// provider identity) should be defined in the consuming application.
type AgentConfig struct {
	MachineName string `json:"MachineName,omitempty"`
	// NodeName is the Kubernetes Node name used by kubelet and host-side
	// daemon watches. During goal-state resolution this field is backfilled
	// once. Resolution prefers an explicitly configured value, then a valid
	// host hostname, then MachineName. Explicit and resolved values must be
	// valid Kubernetes DNS subdomains.
	NodeName string             `json:"NodeName,omitempty"`
	Cluster  AgentClusterConfig `json:"Cluster"`
	Kubelet  AgentKubeletConfig `json:"Kubelet"`
	CRI      CRIConfig          `json:"CRI"`
	CNI      CNIConfig          `json:"CNI"`

	// OCIImage is the fully-qualified OCI image reference (e.g.
	// "ghcr.io/org/repo:tag") used to bootstrap the machine rootfs.
	// When empty the agent falls back to debootstrap.
	OCIImage string `json:"OCIImage,omitempty"`
}

// machineNameEnv is the environment variable that overrides the resolved
// MachineName when the config does not set one. It lets a single bootstrap
// payload (e.g. cloud-init for an Azure VMSS or AWS Auto Scaling group) be
// reused across many instances, each resolving its own name at startup.
const machineNameEnv = "AGENT_MACHINE_NAME"

// BackfillMachineName resolves and stores the MachineName once. It returns the
// source of the resolved value ("config", "env", or "hostname") so callers can
// log the resolution; the source is "config" when an explicit value was already
// present. Resolution order:
//  1. An explicit, valid MachineName already in the config is kept as-is.
//  2. The AGENT_MACHINE_NAME environment variable.
//  3. The host hostname.
//
// Values resolved from the environment or the hostname are normalized (trimmed
// and lowercased) before validation. Callers must invoke BackfillMachineName
// before BackfillNodeName because the latter falls back to MachineName.
func (a *AgentConfig) BackfillMachineName() (string, error) {
	if name := strings.TrimSpace(a.MachineName); name != "" {
		if !isValidNodeName(name) {
			return "", fmt.Errorf("machine name %q is not a valid Kubernetes node name", a.MachineName)
		}

		a.MachineName = name

		return "config", nil
	}

	if env := strings.TrimSpace(os.Getenv(machineNameEnv)); env != "" {
		name, err := normalizeDerivedMachineName(env)
		if err != nil {
			return "", fmt.Errorf("%s: %w", machineNameEnv, err)
		}

		a.MachineName = name

		return "env", nil
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve machine name from hostname: %w", err)
	}

	name, err := normalizeDerivedMachineName(hostname)
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}

	a.MachineName = name

	return "hostname", nil
}

// normalizeDerivedMachineName trims and lowercases a machine name derived from
// the environment or the host hostname, then validates it as a Kubernetes node
// name (DNS-1123 subdomain). It is kept separate from BackfillMachineName so it
// can be unit-tested without depending on os.Hostname.
func normalizeDerivedMachineName(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if !isValidNodeName(normalized) {
		return "", fmt.Errorf("derived machine name %q is not a valid Kubernetes node name", name)
	}

	return normalized, nil
}

// BackfillNodeName resolves and stores the Kubernetes Node name once. An
// explicit NodeName wins when set. Otherwise, a valid host hostname wins, then
// MachineName is used as the final fallback.
func (a *AgentConfig) BackfillNodeName() error {
	if nodeName := strings.TrimSpace(a.NodeName); nodeName != "" {
		if isValidNodeName(nodeName) {
			a.NodeName = nodeName
			return nil
		}

		return fmt.Errorf("node name override %q is not a valid Kubernetes node name", nodeName)
	}

	hostname, hostnameErr := os.Hostname()

	hostNodeName := ""
	if hostnameErr == nil {
		hostNodeName = strings.TrimSpace(hostname)
		if isValidNodeName(hostNodeName) {
			a.NodeName = hostNodeName
			return nil
		}

		hostnameErr = fmt.Errorf("hostname %q is not a valiad Kubenetes node name", hostNodeName)
	}

	nodeName := strings.TrimSpace(a.MachineName)
	if isValidNodeName(nodeName) {
		a.NodeName = nodeName
		return nil
	}

	machineNameErr := fmt.Errorf("machine name %q is not a valid Kubernetse node name", a.MachineName)

	return errors.Join(hostnameErr, machineNameErr)
}

func isValidNodeName(name string) bool {
	if name == "" {
		return false
	}

	return len(validation.IsDNS1123Subdomain(name)) == 0
}

// DeepCopy returns a copy of AgentConfig with mutable nested values cloned.
func (a *AgentConfig) DeepCopy() *AgentConfig {
	if a == nil {
		return nil
	}

	out := *a
	if a.Kubelet.Labels != nil {
		out.Kubelet.Labels = make(map[string]string, len(a.Kubelet.Labels))
		maps.Copy(out.Kubelet.Labels, a.Kubelet.Labels)
	}

	out.Kubelet.RegisterWithTaints = slices.Clone(a.Kubelet.RegisterWithTaints)

	return &out
}

// AgentClusterConfig holds the cluster-level values the agent needs to
// join the Kubernetes control plane.
type AgentClusterConfig struct {
	CaCertBase64 string `json:"CaCertBase64"`
	ClusterDNS   string `json:"ClusterDNS"`
	Version      string `json:"Version"`
}

// AgentKubeletConfig holds kubelet-specific overrides.
type AgentKubeletConfig struct {
	ApiServer string `json:"ApiServer"`
	// NodeIP overrides kubelet --node-ip. Supports a single IP or a
	// comma-separated dual-stack pair.
	NodeIP             string            `json:"NodeIP,omitempty"`
	Auth               KubeletAuthInfo   `json:"Auth"`
	Labels             map[string]string `json:"Labels"`
	RegisterWithTaints []string          `json:"RegisterWithTaints"`
}

// KubeletAuthInfo holds the kubelet authentication configuration.
// Exactly one of BootstrapToken or ExecCredential must be set.
type KubeletAuthInfo struct {
	// BootstrapToken is a Kubernetes bootstrap token in
	// "<token-id>.<token-secret>" format used for TLS bootstrapping.
	BootstrapToken string `json:"BootstrapToken,omitempty"`

	// ExecCredential configures kubelet to authenticate via a
	// client.authentication.k8s.io exec credential plugin.
	ExecCredential *clientcmdapi.ExecConfig `json:"ExecCredential,omitempty"`
}

// Validate checks that exactly one auth method is configured.
func (a *KubeletAuthInfo) Validate() error {
	hasToken := a.BootstrapToken != ""

	hasExec := a.ExecCredential != nil
	switch {
	case hasToken && hasExec:
		return fmt.Errorf("BootstrapToken and ExecCredential are mutually exclusive")
	case !hasToken && !hasExec:
		return fmt.Errorf("one of BootstrapToken or ExecCredential must be set")
	}

	if hasExec && a.ExecCredential.Command == "" {
		return fmt.Errorf("ExecCredential.Command is required")
	}

	return nil
}

// CRIConfig holds container runtime version overrides. Zero values fall
// back to the library defaults in goalstates/constants.go.
type CRIConfig struct {
	Containerd ContainerdConfig `json:"Containerd"`
	Runc       RuncConfig       `json:"Runc"`
}

// ContainerdConfig holds containerd-specific overrides.
type ContainerdConfig struct {
	// Version overrides the default containerd version (e.g. "2.0.4").
	Version string `json:"Version,omitempty"`
}

// RuncConfig holds runc-specific overrides.
type RuncConfig struct {
	// Version overrides the default runc version (e.g. "1.1.12").
	Version string `json:"Version,omitempty"`
}

// CNIConfig holds CNI plugin version overrides. Zero values fall back to
// the library defaults in goalstates/constants.go.
type CNIConfig struct {
	// PluginVersion overrides the default CNI plugin version (e.g. "1.5.1").
	PluginVersion string `json:"PluginVersion,omitempty"`
}
