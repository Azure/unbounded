// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// WorkloadConfig contains only the inputs needed to build the dataplane DaemonSet.
// Controller limits, rotation policy, serving TLS, and durable state are independent.
type WorkloadConfig struct {
	Cluster                 wire.ClusterID
	Namespace               string
	ControlURL              string
	BootstrapTrustConfigMap string
	DataplaneImage          string
	PeerPort                uint16
	HostNetwork             bool
	// Zero preserves the legacy automatic diagnostics port (9090, or 9091).
	DiagnosticsPort         uint16
	DataplaneServiceAccount string
	DaemonSetName           string
}

// WorkloadConfigFromLookup reads operator deployment wiring without loading or
// validating controller runtime configuration. Shared settings retain the same defaults.
func WorkloadConfigFromLookup(lookup func(string) (string, bool)) (WorkloadConfig, error) {
	env := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}

		return fallback
	}

	port, err := strconv.ParseUint(env("RACER_PEER_PORT", "8082"), 10, 16)
	if err != nil {
		return WorkloadConfig{}, fmt.Errorf("RACER_PEER_PORT: %w", wire.InvalidRequest)
	}

	hostNetwork := env("RACER_HOST_NETWORK", "false")
	if hostNetwork != "true" && hostNetwork != "false" {
		return WorkloadConfig{}, fmt.Errorf("RACER_HOST_NETWORK must be true or false: %w", wire.InvalidRequest)
	}

	var diagnosticsPort uint64
	if value, ok := lookup("RACER_DIAGNOSTICS_PORT"); ok {
		diagnosticsPort, err = strconv.ParseUint(value, 10, 16)
		if err != nil || diagnosticsPort < 1024 {
			return WorkloadConfig{}, fmt.Errorf("RACER_DIAGNOSTICS_PORT must be 1024..65535: %w", wire.InvalidRequest)
		}
	}

	cfg := WorkloadConfig{
		Cluster:                 wire.ClusterID(env("RACER_CLUSTER_ID", "")),
		Namespace:               env("POD_NAMESPACE", "unbounded-system"),
		ControlURL:              env("RACER_CONTROL_URL", ""),
		BootstrapTrustConfigMap: env("RACER_BOOTSTRAP_TRUST_CONFIGMAP", "racer-bootstrap-trust"),
		DataplaneImage:          env("RACER_DATAPLANE_IMAGE", ""),
		PeerPort:                uint16(port),
		HostNetwork:             hostNetwork == "true",
		DiagnosticsPort:         uint16(diagnosticsPort),
		DataplaneServiceAccount: env("RACER_DATAPLANE_SERVICE_ACCOUNT", "racer-dataplane"),
		DaemonSetName:           env("RACER_DAEMONSET_NAME", "racer-dataplane"),
	}

	return cfg, cfg.Validate()
}

func (c WorkloadConfig) Validate() error {
	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.PeerPort < 1024 {
		return fmt.Errorf("cluster, namespace, or peer port: %w", wire.InvalidRequest)
	}

	if c.DiagnosticsPort != 0 && (c.DiagnosticsPort < 1024 || c.DiagnosticsPort == c.PeerPort) {
		return fmt.Errorf("diagnostics port must be 1024..65535 and distinct from peer port: %w", wire.InvalidRequest)
	}

	for _, name := range []string{c.DaemonSetName, c.BootstrapTrustConfigMap, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(c.DataplaneImage) == "" {
		return fmt.Errorf("workload endpoint or image: %w", wire.InvalidRequest)
	}

	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("workload endpoint port: %w", wire.InvalidRequest)
		}
	}

	return nil
}
