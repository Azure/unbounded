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
	"net/url"
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
	MachineName string `json:"MachineName"`
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

	out.CRI.Containerd.RegistryMirrors = slices.Clone(a.CRI.Containerd.RegistryMirrors)

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
	// Version overrides the default containerd version (e.g. "2.1.8").
	Version string `json:"Version,omitempty"`

	// RegistryMirrors configures containerd registry mirror hosts. For
	// each entry the agent writes a hosts.toml file under
	// /etc/containerd/certs.d/<Host>/ in the worker rootfs, pointing the
	// registry at the configured Mirror endpoint. This is the containerd
	// mechanism a local pull-through cache such as Gantry plugs into.
	RegistryMirrors []ContainerdRegistryMirror `json:"RegistryMirrors,omitempty"`
}

// ContainerdRegistryMirror describes a single containerd registry mirror.
// It maps to a /etc/containerd/certs.d/<Host>/hosts.toml file in the worker
// node rootfs.
type ContainerdRegistryMirror struct {
	// Host is the canonical registry hostname used as the certs.d
	// directory name (e.g. "registry.k8s.io", "index.docker.io"). It must
	// be a bare host or host:port with no scheme or path.
	Host string `json:"Host"`

	// Server is the upstream registry URL containerd falls back to when
	// the mirror cannot serve a request (e.g. "https://registry.k8s.io").
	Server string `json:"Server"`

	// Mirror is the mirror endpoint containerd tries first (e.g.
	// "http://127.0.0.1:5000" for a node-local Gantry pod).
	Mirror string `json:"Mirror"`

	// SkipVerify disables TLS verification for the mirror endpoint. This
	// is typically set for a loopback http mirror.
	SkipVerify bool `json:"SkipVerify,omitempty"`
}

// Validate checks that a registry mirror has a well-formed host, server URL,
// and mirror URL. It rejects malformed inputs rather than letting them flow
// into a certs.d directory path.
func (m *ContainerdRegistryMirror) Validate() error {
	if err := validateRegistryHost(m.Host); err != nil {
		return fmt.Errorf("invalid Host %q: %w", m.Host, err)
	}

	if err := validateAbsoluteHTTPURL(m.Server); err != nil {
		return fmt.Errorf("invalid Server %q: %w", m.Server, err)
	}

	if err := validateAbsoluteHTTPURL(m.Mirror); err != nil {
		return fmt.Errorf("invalid Mirror %q: %w", m.Mirror, err)
	}

	return nil
}

// ValidateRegistryMirrors validates every mirror and rejects duplicate hosts.
func ValidateRegistryMirrors(mirrors []ContainerdRegistryMirror) error {
	seen := make(map[string]struct{}, len(mirrors))

	for i := range mirrors {
		if err := mirrors[i].Validate(); err != nil {
			return fmt.Errorf("registry mirror %d: %w", i, err)
		}

		host := mirrors[i].Host
		if _, dup := seen[host]; dup {
			return fmt.Errorf("registry mirror %d: duplicate Host %q", i, host)
		}

		seen[host] = struct{}{}
	}

	return nil
}

// validateRegistryHost ensures host is a bare hostname or host:port with no
// scheme or path component, so it is safe to use as a certs.d directory name.
func validateRegistryHost(host string) error {
	if host == "" {
		return errors.New("must not be empty")
	}

	if host == "." || host == ".." {
		return errors.New("must be a safe certs.d path segment")
	}

	if strings.Contains(host, "://") || strings.Contains(host, "/") {
		return errors.New("must be a bare host or host:port with no scheme or path")
	}

	// Parse as a network-path reference so an optional :port is handled.
	u, err := url.Parse("//" + host)
	if err != nil {
		return fmt.Errorf("invalid host: %w", err)
	}

	if u.Host != host || u.Path != "" {
		return errors.New("must be a bare host or host:port with no scheme or path")
	}

	return nil
}

// validateAbsoluteHTTPURL ensures raw is an absolute http or https URL with a
// host component.
func validateAbsoluteHTTPURL(raw string) error {
	if raw == "" {
		return errors.New("must not be empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an http or https URL")
	}

	if u.Host == "" {
		return errors.New("must include a host")
	}

	return nil
}

// RuncConfig holds runc-specific overrides.
type RuncConfig struct {
	// Version overrides the default runc version (e.g. "1.5.0").
	Version string `json:"Version,omitempty"`
}

// CNIConfig holds CNI plugin version overrides. Zero values fall back to
// the library defaults in goalstates/constants.go.
type CNIConfig struct {
	// PluginVersion overrides the default CNI plugin version (e.g. "1.5.1").
	PluginVersion string `json:"PluginVersion,omitempty"`
}
