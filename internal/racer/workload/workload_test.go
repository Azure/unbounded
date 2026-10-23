// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testConfig() Config {
	return DefaultConfig("custom-system", "registry.test/racer-dataplane:v1", "registry.test/racer-object:v1")
}

func envMap(t *testing.T, c corev1.Container) map[string]corev1.EnvVar {
	t.Helper()

	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		if _, dup := out[e.Name]; dup {
			t.Fatalf("duplicate env %s", e.Name)
		}

		out[e.Name] = e
	}

	return out
}

func TestDaemonSetRuntimeContract(t *testing.T) {
	ds, err := DaemonSet(testConfig())
	if err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.ServiceAccountName != "racer-dataplane" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("service account wiring = %q %v", pod.ServiceAccountName, pod.AutomountServiceAccountToken)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Containers))
	}

	env := envMap(t, pod.Containers[0])
	for name, path := range map[string]string{
		"RACER_NAMESPACE": "metadata.namespace", "RACER_POD_NAME": "metadata.name", "RACER_POD_UID": "metadata.uid",
		"RACER_NODE_NAME": "spec.nodeName", "RACER_POD_IP": "status.podIP",
	} {
		if e := env[name]; e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != path {
			t.Fatalf("%s = %+v, want downward %s", name, e, path)
		}
	}

	for name, value := range map[string]string{
		"RACER_CONTROL_LISTEN":        "[$(RACER_POD_IP)]:8443",
		"RACER_WEBHOOK_LISTEN":        "[$(RACER_POD_IP)]:9443",
		"RACER_PEER_LISTEN":           "[$(RACER_POD_IP)]:8082",
		"RACER_DIAGNOSTICS_LISTEN":    "[$(RACER_POD_IP)]:9090",
		"RACER_OBJECT_IMAGE":          "registry.test/racer-object:v1",
		"RACER_SERVICE_ACCOUNT_TOKEN": "/var/run/racer-token/token",
	} {
		if env[name].Value != value {
			t.Fatalf("%s = %q, want %q", name, env[name].Value, value)
		}
	}

	// The runtime derives the cluster from the state Secret and discovers
	// seats through Leases; no controller wiring may remain.
	for _, name := range []string{ControlSeatsKey, "RACER_CLUSTER_ID", "RACER_CONTROL_ENDPOINT", "RACER_TRUST_BUNDLE"} {
		if _, ok := env[name]; ok {
			t.Fatalf("obsolete env %s present", name)
		}
	}

	// The Pod IP is defined before every listener that expands it.
	for i, e := range pod.Containers[0].Env {
		if e.Name == "RACER_POD_IP" {
			break
		}

		if e.Value != "" {
			t.Fatalf("env %d (%s) precedes RACER_POD_IP", i, e.Name)
		}
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}

	if _, ok := volumes["bootstrap"]; ok {
		t.Fatal("bootstrap CA volume must not be mounted")
	}

	api := volumes["kube-api-access"].Projected
	if api == nil || len(api.Sources) != 3 || api.Sources[0].ServiceAccountToken == nil || api.Sources[0].ServiceAccountToken.Audience != "" ||
		api.Sources[1].ConfigMap == nil || api.Sources[1].ConfigMap.Name != "kube-root-ca.crt" ||
		api.Sources[2].DownwardAPI == nil || api.Sources[2].DownwardAPI.Items[0].Path != "namespace" {
		t.Fatalf("API credentials = %+v", api)
	}

	control := volumes["token"].Projected
	if control == nil || len(control.Sources) != 1 || control.Sources[0].ServiceAccountToken.Audience != "racer-control" {
		t.Fatalf("control token = %+v", control)
	}

	mounts := map[string]string{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.Name] = m.MountPath
	}

	if mounts["kube-api-access"] != "/var/run/secrets/kubernetes.io/serviceaccount" || mounts["token"] != "/var/run/racer-token" {
		t.Fatalf("mounts = %v", mounts)
	}

	for _, name := range []string{"identity", "slabs", "sockets", "devices", "infiniband"} {
		if mounts[name] == "" {
			t.Fatalf("missing mount %s", name)
		}
	}

	ports := map[string]int32{}
	for _, p := range pod.Containers[0].Ports {
		ports[p.Name] = p.ContainerPort
	}

	if ports["peer"] != 8082 || ports["diagnostics"] != 9090 || ports["control"] != 8443 || ports["webhook"] != 9443 {
		t.Fatalf("ports = %v", ports)
	}
}

func TestDaemonSetHostNetworkAndDiagnosticsPort(t *testing.T) {
	cfg := testConfig()
	cfg.HostNetwork, cfg.PeerPort = true, 9090

	ds, err := DaemonSet(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !ds.Spec.Template.Spec.HostNetwork || ds.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Fatal("host network not applied")
	}

	if envMap(t, ds.Spec.Template.Spec.Containers[0])["RACER_DIAGNOSTICS_LISTEN"].Value != "[$(RACER_POD_IP)]:9091" {
		t.Fatal("diagnostics port must move off a 9090 peer port")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"namespace":         func(c *Config) { c.Namespace = "Bad_NS" },
		"dataplane image":   func(c *Config) { c.DataplaneImage = " " },
		"object image":      func(c *Config) { c.ObjectImage = "" },
		"privileged peer":   func(c *Config) { c.PeerPort = 80 },
		"privileged diag":   func(c *Config) { c.DiagnosticsPort = 80 },
		"peer is control":   func(c *Config) { c.PeerPort = ControlPort },
		"diag is webhook":   func(c *Config) { c.DiagnosticsPort = WebhookPort },
		"diag equals peer":  func(c *Config) { c.DiagnosticsPort = c.PeerPort },
		"peer equals diag0": func(c *Config) { c.DiagnosticsPort = 9090; c.PeerPort = 9090 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)

			if _, err := DaemonSet(cfg); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDaemonSetRequestsMemoryWithoutLimit(t *testing.T) {
	ds, err := DaemonSet(testConfig())
	if err != nil {
		t.Fatal(err)
	}

	resources := ds.Spec.Template.Spec.Containers[0].Resources
	if got := resources.Requests.Memory().String(); got != DataplaneMemoryRequest {
		t.Fatalf("memory request = %q, want %q", got, DataplaneMemoryRequest)
	}

	if len(resources.Limits) != 0 {
		t.Fatalf("dataplane must not set a default limit, got %v", resources.Limits)
	}
}
