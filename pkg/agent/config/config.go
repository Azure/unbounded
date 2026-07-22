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
	"path/filepath"
	"slices"
	"strings"
	"unicode"

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
	Gantry   *GantryConfig      `json:"Gantry,omitempty"`

	// OCIImage is an OCI registry reference, local OCI layout, or HTTPS URL
	// to a tarred OCI image layout used to bootstrap the machine rootfs. HTTPS
	// archives must contain exactly one tagged image reference, which the agent
	// selects automatically. HTTPS URLs may contain signed query parameters.
	// When empty the agent uses the built-in default image.
	OCIImage string `json:"OCIImage,omitempty"`

	// AdditionalHostDevices lists extra host device nodes under /dev or systemd
	// device group specifiers that should be exposed to the nspawn machine in
	// addition to automatically discovered devices.
	AdditionalHostDevices []string `json:"AdditionalHostDevices,omitempty"`

	// AdditionalHostMounts lists extra host paths that should be bind-mounted
	// into the nspawn machine. ReadOnly should be used unless write access is
	// required.
	AdditionalHostMounts []AdditionalHostMount `json:"AdditionalHostMounts,omitempty"`

	// OfflineArtifacts points to a complete offline binary artifact source.
	// When set, it takes precedence over download overrides. Source is rendered
	// as a strict Go template using the cluster Kubernetes version, then
	// resolved as an absolute filesystem path, file:// URL, HTTPS archive, or
	// oci:// artifact reference. HTTPS URLs may contain signed query parameters.
	OfflineArtifacts *AgentOfflineArtifacts `json:"OfflineArtifacts,omitempty"`
}

// AgentOfflineArtifacts configures a complete offline source for binaries the
// agent installs into the nspawn rootfs.
type AgentOfflineArtifacts struct {
	Source string `json:"Source,omitempty"`
}

// GantryConfig holds optional Gantry integration settings.
type GantryConfig struct {
	// Disabled skips writing the default containerd hosts.toml that points at
	// Gantry. This is a breakglass setting for environments that need to own
	// containerd registry routing independently.
	Disabled bool `json:"Disabled,omitempty"`
}

// AdditionalHostMount configures a host path bind mount for the nspawn
// machine. Target defaults to Source when omitted.
type AdditionalHostMount struct {
	Source   string `json:"Source"`
	Target   string `json:"Target,omitempty"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// DeepCopy returns a copy of AgentOfflineArtifacts.
func (a *AgentOfflineArtifacts) DeepCopy() *AgentOfflineArtifacts {
	if a == nil {
		return nil
	}

	out := *a

	return &out
}

// OfflineArtifactsConfigured reports whether an offline artifact source is configured.
func (a *AgentConfig) OfflineArtifactsConfigured() bool {
	return a != nil && a.OfflineArtifacts != nil && strings.TrimSpace(a.OfflineArtifacts.Source) != ""
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
	out.AdditionalHostDevices = slices.Clone(a.AdditionalHostDevices)

	out.AdditionalHostMounts = slices.Clone(a.AdditionalHostMounts)
	if a.Gantry != nil {
		out.Gantry = &GantryConfig{Disabled: a.Gantry.Disabled}
	}

	out.OfflineArtifacts = a.OfflineArtifacts.DeepCopy()

	return &out
}

// Validate checks that required agent configuration fields are present and
// internally consistent. Kubelet auth is validated when present; callers that
// require a bootstrap credential should enforce that separately because some
// flows fill the credential later through attestation.
func (a *AgentConfig) Validate() error {
	if a == nil {
		return fmt.Errorf("agent config is nil")
	}

	var errs []error
	if strings.TrimSpace(a.MachineName) == "" {
		errs = append(errs, fmt.Errorf("MachineName is required"))
	}

	if nodeName := strings.TrimSpace(a.NodeName); nodeName == "" {
		errs = append(errs, fmt.Errorf("NodeName is required"))
	} else if !isValidNodeName(nodeName) {
		errs = append(errs, fmt.Errorf("NodeName is not a valid Kubernetes node name"))
	}

	if strings.TrimSpace(a.Cluster.ClusterDNS) == "" {
		errs = append(errs, fmt.Errorf("Cluster.ClusterDNS is required"))
	}

	if err := ValidateAdditionalHostDevices(a.AdditionalHostDevices); err != nil {
		errs = append(errs, err)
	}

	if err := ValidateAdditionalHostMounts(a.AdditionalHostMounts); err != nil {
		errs = append(errs, err)
	}

	apiServer := strings.TrimSpace(a.Kubelet.ApiServer)
	if apiServer == "" {
		errs = append(errs, fmt.Errorf("Kubelet.ApiServer is required"))
	} else if u, err := url.Parse(apiServer); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("Kubelet.ApiServer is invalid"))
	}

	// Kubelet auth is intentionally not validated here. Some consumers provide
	// credentials later through product-specific flows such as attestation, and
	// that context is outside the shared AgentConfig. Callers that require a
	// static bootstrap credential should validate Kubelet.Auth separately.

	return errors.Join(errs...)
}

// ValidateAdditionalHostDevices checks that configured host device paths and
// systemd device group specifiers are safe to render into nspawn directives.
func ValidateAdditionalHostDevices(paths []string) error {
	var errs []error

	for _, path := range paths {
		if err := validateAdditionalHostDevice(path); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func validateAdditionalHostDevice(path string) error {
	if path != strings.TrimSpace(path) || strings.ContainsAny(path, " \t\r\n:") {
		return fmt.Errorf("AdditionalHostDevices entry %q must not contain whitespace or ':'", path)
	}

	if IsSystemdDeviceGroupSpecifier(path) {
		return nil
	}

	if path == "" || !strings.HasPrefix(path, "/dev/") {
		return fmt.Errorf("AdditionalHostDevices entry %q must be an absolute path under /dev or a systemd device group specifier", path)
	}

	if cleaned := filepath.Clean(path); cleaned != path || !strings.HasPrefix(cleaned, "/dev/") {
		return fmt.Errorf("AdditionalHostDevices entry %q must be a clean absolute path under /dev", path)
	}

	return nil
}

// ValidateAdditionalHostMounts checks that configured host bind-mount paths
// are safe to render into nspawn directives.
func ValidateAdditionalHostMounts(mounts []AdditionalHostMount) error {
	var errs []error

	for i, mount := range mounts {
		if err := validateAdditionalHostMountPath(mount.Source); err != nil {
			errs = append(errs, fmt.Errorf("AdditionalHostMounts[%d].Source: %w", i, err))
		}

		if mount.Target != "" {
			if err := validateAdditionalHostMountPath(mount.Target); err != nil {
				errs = append(errs, fmt.Errorf("AdditionalHostMounts[%d].Target: %w", i, err))
			}
		}
	}

	return errors.Join(errs...)
}

func validateAdditionalHostMountPath(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("%q must be an absolute path", path)
	}

	if strings.IndexFunc(path, func(r rune) bool {
		return r == ':' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("%q must not contain whitespace, control characters, or ':'", path)
	}

	if cleaned := filepath.Clean(path); cleaned != path {
		return fmt.Errorf("%q must be a clean absolute path", path)
	}

	return nil
}

// IsSystemdDeviceGroupSpecifier reports whether value is a systemd DeviceAllow
// device group specifier, such as char-input or block-*. Group names accept
// ASCII letters, digits, underscores, hyphens, and the '*' wildcard only.
func IsSystemdDeviceGroupSpecifier(value string) bool {
	for _, prefix := range []string{"char-", "block-"} {
		if !strings.HasPrefix(value, prefix) {
			continue
		}

		group := strings.TrimPrefix(value, prefix)

		return group != "" && strings.IndexFunc(group, func(r rune) bool {
			return (r < 'a' || r > 'z') &&
				(r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') &&
				r != '_' && r != '-' && r != '*'
		}) == -1
	}

	return false
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

	// SandboxImage overrides the CRI sandbox image used by containerd
	// (e.g. "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9").
	SandboxImage string `json:"SandboxImage,omitempty"`
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
