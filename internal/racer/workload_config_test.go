// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestWorkloadConfigIdentityAndNames(t *testing.T) {
	for name, mutate := range map[string]func(*WorkloadConfig){
		"cluster":           func(c *WorkloadConfig) { c.Cluster = "invalid" },
		"missing cluster":   func(c *WorkloadConfig) { c.Cluster = "" },
		"namespace":         func(c *WorkloadConfig) { c.Namespace = "invalid.namespace" },
		"missing namespace": func(c *WorkloadConfig) { c.Namespace = "" },
		"zero port":         func(c *WorkloadConfig) { c.PeerPort = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := workloadConfig(t)
			mutate(&cfg)

			if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
				t.Fatalf("invalid workload config accepted: %v", err)
			}
		})
	}

	for _, name := range []string{"daemonset", "keyring", "trust", "serviceaccount"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "../name", "Uppercase", strings.Repeat("a", 254)} {
				cfg := workloadConfig(t)
				fields := map[string]*string{
					"daemonset": &cfg.DaemonSetName, "keyring": &cfg.KeyringSecretName,
					"trust": &cfg.BootstrapTrustConfigMap, "serviceaccount": &cfg.DataplaneServiceAccount,
				}

				*fields[name] = value
				if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
					t.Fatalf("invalid resource name %q accepted: %v", value, err)
				}
			}
		})
	}
}

func TestWorkloadConfigLookupDefaultsAndOverrides(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:test",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := WorkloadConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Namespace != runtime.Namespace || cfg.PeerPort != runtime.PeerPort || cfg.DaemonSetName != runtime.DaemonSetName || cfg.DataplaneServiceAccount != runtime.DataplaneServiceAccount || cfg.KeyringSecretName != runtime.KeyringSecretName || cfg.BootstrapTrustConfigMap != "racer-bootstrap-trust" {
		t.Fatalf("workload defaults disagree with runtime: %+v", cfg)
	}

	for key, value := range map[string]string{
		"POD_NAMESPACE": "custom", "RACER_PEER_PORT": "65535", "RACER_DAEMONSET_NAME": "custom.dataplane",
		"RACER_DATAPLANE_SERVICE_ACCOUNT": "custom.account", "RACER_KEYRING_SECRET_NAME": "custom.keyring",
		"RACER_BOOTSTRAP_TRUST_CONFIGMAP": "custom.trust",
	} {
		values[key] = value
	}

	for key := range values {
		t.Setenv(key, "invalid-process-value")
	}

	cfg, err = WorkloadConfigFromLookup(lookup)

	want := WorkloadConfig{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "custom", PeerPort: 65535,
		ControlURL: "https://controller:8443", DataplaneImage: "racer:test", DaemonSetName: "custom.dataplane",
		DataplaneServiceAccount: "custom.account", KeyringSecretName: "custom.keyring", BootstrapTrustConfigMap: "custom.trust",
	}
	if err != nil || cfg != want {
		t.Fatalf("custom lookup: %+v, %v", cfg, err)
	}

	for key := range values {
		previous := values[key]
		for _, invalid := range []string{"", "invalid value"} {
			values[key] = invalid
			// Image syntax remains the container runtime's responsibility.
			if key == "RACER_DATAPLANE_IMAGE" && invalid != "" {
				continue
			}

			if _, err := WorkloadConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
				t.Fatalf("%s=%q accepted: %v", key, invalid, err)
			}
		}

		values[key] = previous
	}

	for _, port := range []string{"0", "65536", "-1"} {
		values["RACER_PEER_PORT"] = port
		if _, err := WorkloadConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("port %q accepted: %v", port, err)
		}
	}
}

func TestWorkloadConfigIgnoresControllerRuntime(t *testing.T) {
	want := workloadConfig(t)
	values := map[string]string{
		"RACER_CLUSTER_ID": string(want.Cluster), "POD_NAMESPACE": want.Namespace,
		"RACER_CONTROL_URL": want.ControlURL, "RACER_DATAPLANE_IMAGE": want.DataplaneImage,
	}

	cfg, err := WorkloadConfigFromLookup(func(key string) (string, bool) {
		if value, ok := values[key]; ok {
			return value, true
		}

		switch key {
		case "RACER_PEER_PORT", "RACER_DATAPLANE_SERVICE_ACCOUNT", "RACER_DAEMONSET_NAME", "RACER_KEYRING_SECRET_NAME", "RACER_BOOTSTRAP_TRUST_CONFIGMAP":
			return "", false
		default:
			t.Errorf("workload parser requested runtime setting %s", key)
			return "invalid", true
		}
	})
	if err != nil || cfg != want {
		t.Fatalf("workload needs runtime configuration: %+v, %v", cfg, err)
	}

	if _, err := DesiredDaemonSet(cfg); err != nil {
		t.Fatal(err)
	}
}
