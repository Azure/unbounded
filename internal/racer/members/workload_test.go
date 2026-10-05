// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"errors"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func workloadConfig(t *testing.T) Config {
	t.Helper()

	return Config{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "racer",
		ControlURL: "https://racer-controller.racer.svc:8443", DataplaneImage: "racer:test",
		BootstrapTrustConfigMap: "racer-bootstrap-trust", PeerPort: 8082,
		DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane",
	}
}

func TestWorkloadProjectionAndStorage(t *testing.T) {
	cfg := workloadConfig(t)

	ds, err := DesiredDaemonSet(cfg)
	if err != nil {
		t.Fatal(err)
	}

	wantSelector := map[string]string{"app.kubernetes.io/name": "racer-dataplane", "app.kubernetes.io/instance": cfg.DaemonSetName}
	if !reflect.DeepEqual(ds.Spec.Selector.MatchLabels, wantSelector) || !reflect.DeepEqual(ds.Spec.Template.Labels, wantSelector) {
		t.Fatalf("unexpected fresh workload selector: %v", ds.Spec.Selector)
	}

	pod := ds.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != cfg.DataplaneServiceAccount {
		t.Fatal("automatic API token or wrong service account")
	}

	volumes := map[string]corev1.Volume{}

	for _, v := range pod.Volumes {
		if v.Secret != nil {
			t.Fatal("dataplane must fetch shared keys over control HTTPS, not mount Secrets")
		}

		if v.Projected != nil {
			for _, source := range v.Projected.Sources {
				if source.Secret != nil {
					t.Fatal("dataplane must not project Secrets")
				}
			}
		}

		volumes[v.Name] = v
	}

	token := volumes["token"].Projected.Sources[0].ServiceAccountToken
	if token.Audience != wire.TokenAudience || *token.ExpirationSeconds != 3600 {
		t.Fatal("wrong token projection")
	}

	if _, exists := volumes["keyring"]; exists {
		t.Fatal("shared keyring volume must not exist")
	}

	if volumes["bootstrap"].ConfigMap.Name != cfg.BootstrapTrustConfigMap {
		t.Fatal("bootstrap trust not independent")
	}

	if !reflect.DeepEqual(volumes["bootstrap"].ConfigMap.Items, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}) {
		t.Fatal("only controller CA trust may be projected as cryptographic material")
	}

	for _, name := range []string{"identity", "slabs", "sockets", "infiniband"} {
		if volumes[name].HostPath == nil || *volumes[name].HostPath.Type != corev1.HostPathDirectoryOrCreate {
			t.Fatalf("missing persistent host mount %s", name)
		}
	}

	if volumes["identity"].HostPath.Path == volumes["slabs"].HostPath.Path {
		t.Fatal("private keys share disposable storage")
	}

	requirements := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if requirements[0].Key != wire.ExclusionLabel || requirements[0].Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Fatal("exclusion must test presence")
	}

	for _, env := range pod.Containers[0].Env {
		if env.ValueFrom != nil && (env.Name != "RACER_POD_IP" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}})) {
			t.Fatal("node identity must come from verified enrollment")
		}
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range pod.Containers[0].VolumeMounts {
		mounts[mount.Name] = mount
		if mount.SubPath != "" {
			t.Fatal("subPath prevents projection rotation")
		}

		if (mount.Name == "token" || mount.Name == "bootstrap") && !mount.ReadOnly {
			t.Fatal("writable credential projection")
		}
	}

	wantMounts := map[string]corev1.VolumeMount{
		"token":      {Name: "token", MountPath: "/var/run/racer-token", ReadOnly: true},
		"bootstrap":  {Name: "bootstrap", MountPath: "/etc/racer/bootstrap", ReadOnly: true},
		"identity":   {Name: "identity", MountPath: "/var/lib/racer/identity"},
		"slabs":      {Name: "slabs", MountPath: "/var/lib/racer/slabs"},
		"sockets":    {Name: "sockets", MountPath: "/run/racer"},
		"infiniband": {Name: "infiniband", MountPath: "/dev/infiniband", ReadOnly: true},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Fatal("mounts must retain token, controller trust, private identity, slabs, sockets and RDMA devices only")
	}

	if len(pod.Containers[0].Args) != 0 || len(pod.Containers[0].Command) != 0 {
		t.Fatal("workload must use the image entrypoint")
	}
}

func TestWorkloadNativeRDMAAccess(t *testing.T) {
	for _, hostNetwork := range []bool{false, true} {
		t.Run(strconv.FormatBool(hostNetwork), func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.HostNetwork = hostNetwork

			ds, err := DesiredDaemonSet(cfg)
			if err != nil {
				t.Fatal(err)
			}

			pod := ds.Spec.Template.Spec
			if pod.HostNetwork != hostNetwork || pod.HostPID || pod.HostIPC {
				t.Fatal("RDMA access must not implicitly enable host namespaces")
			}

			wantSecurity := &corev1.SecurityContext{
				Privileged: ptr.To(true), AllowPrivilegeEscalation: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(true),
			}
			if !reflect.DeepEqual(pod.Containers[0].SecurityContext, wantSecurity) || !reflect.DeepEqual(pod.SecurityContext, &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))}) {
				t.Fatal("native dataplane must explicitly run privileged as root with a read-only rootfs")
			}

			wantVolume := corev1.Volume{Name: "infiniband", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/infiniband", Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			}}}

			if len(pod.Volumes) != 6 {
				t.Fatalf("unexpected volumes: %v", pod.Volumes)
			}

			found := false

			for _, volume := range pod.Volumes {
				if volume.Name == "infiniband" {
					found = reflect.DeepEqual(volume, wantVolume)
				}
			}

			if !found {
				t.Fatal("RDMA hostPath must tolerate an absent directory on HTTP-only nodes")
			}
		})
	}
}

func TestWorkloadDataplaneEnvironment(t *testing.T) {
	for _, port := range []uint16{8082, 7443, 9090, 9091, 65535} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.PeerPort = port

			ds, err := DesiredDaemonSet(cfg)
			if err != nil {
				t.Fatal(err)
			}

			container := ds.Spec.Template.Spec.Containers[0]

			env := map[string]string{}
			for _, value := range container.Env {
				if _, exists := env[value.Name]; exists {
					t.Fatalf("duplicate configuration: %s", value.Name)
				}

				env[value.Name] = value.Value
			}

			diagnosticsPort := "9090"
			if port == 9090 {
				diagnosticsPort = "9091"
			}
			// Rust Config::from_lookup consumes these settings after kubelet expands
			// the Pod IP helper into both listener addresses.
			expected := map[string]string{
				"RACER_CLUSTER_ID":            string(cfg.Cluster),
				"RACER_CONTROL_ENDPOINT":      cfg.ControlURL,
				"RACER_PEER_LISTEN":           "[$(RACER_POD_IP)]:" + strconv.Itoa(int(port)),
				"RACER_POD_IP":                "",
				"RACER_DIAGNOSTICS_LISTEN":    "[$(RACER_POD_IP)]:" + diagnosticsPort,
				"RACER_TRUST_BUNDLE":          "/etc/racer/bootstrap/ca.crt",
				"RACER_SERVICE_ACCOUNT_TOKEN": "/var/run/racer-token/token",
				"RACER_IDENTITY_DIRECTORY":    "/var/lib/racer/identity/private",
				"RACER_SLAB_DIRECTORY":        "/var/lib/racer/slabs",
			}
			if len(env) != len(expected) {
				t.Fatalf("unexpected configuration: %v", env)
			}

			for name, value := range expected {
				if env[name] != value {
					t.Errorf("%s = %q, want %q", name, env[name], value)
				}
			}

			if container.Ports[0].ContainerPort != int32(port) {
				t.Fatal("listener disagrees with advertised peer port")
			}

			assertWorkloadReadiness(t, ds)

			mounts := map[string]string{}
			for _, mount := range container.VolumeMounts {
				mounts[mount.Name] = mount.MountPath
			}

			for name, location := range map[string]string{
				"RACER_TRUST_BUNDLE":          path.Join(mounts["bootstrap"], "ca.crt"),
				"RACER_SERVICE_ACCOUNT_TOKEN": path.Join(mounts["token"], "token"),
				"RACER_IDENTITY_DIRECTORY":    path.Join(mounts["identity"], "private"),
				"RACER_SLAB_DIRECTORY":        mounts["slabs"],
			} {
				if env[name] != location {
					t.Errorf("%s does not match its mounted projection or storage", name)
				}
			}
		})
	}
}

func assertWorkloadReadiness(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()

	rolling := ds.Spec.UpdateStrategy.RollingUpdate
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType || rolling == nil || rolling.MaxUnavailable == nil || *rolling.MaxUnavailable != intstr.FromInt32(1) || rolling.MaxSurge == nil || *rolling.MaxSurge != intstr.FromInt32(0) || ds.Spec.MinReadySeconds != 10 {
		t.Fatal("rollout must wait for sustained readiness with at most one unavailable Pod")
	}

	container := ds.Spec.Template.Spec.Containers[0]

	expected := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("diagnostics"), Scheme: corev1.URISchemeHTTP}},
		PeriodSeconds: 5, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 1,
	}
	if !reflect.DeepEqual(container.ReadinessProbe, expected) || container.LivenessProbe != nil || container.StartupProbe != nil {
		t.Fatal("must probe actual Pod-IP readiness without dependency-driven restarts")
	}

	ports := map[string]int32{}

	for _, port := range container.Ports {
		if port.Protocol != corev1.ProtocolTCP || port.HostPort != 0 || port.HostIP != "" {
			t.Fatal("listeners must use Pod TCP ports")
		}

		ports[port.Name] = port.ContainerPort
	}

	if ports["diagnostics"] == 0 || ports["diagnostics"] == ports["peer"] {
		t.Fatal("diagnostics missing or collides with peer listener")
	}

	podIPSeen, diagnosticsSeen := false, false

	for _, env := range container.Env {
		switch env.Name {
		case "RACER_POD_IP":
			if podIPSeen || env.Value != "" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}) {
				t.Fatal("bind address must come from the downward API Pod IP")
			}

			podIPSeen = true
		case "RACER_DIAGNOSTICS_LISTEN":
			if !podIPSeen || diagnosticsSeen || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(ports["diagnostics"])) {
				t.Fatal("diagnostics must expand the preceding Pod IP and match the probe port")
			}

			diagnosticsSeen = true
		default:
			if env.ValueFrom != nil {
				t.Fatal("unexpected indirect configuration")
			}
		}
	}

	if !podIPSeen || !diagnosticsSeen {
		t.Fatal("missing diagnostics bind configuration")
	}
}

func TestDesiredDaemonSetRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "http://host", "https://host:0", "https://host:65536", "https://user@host", "https://host/path", "https://host?", "https://host/#fragment"} {
		cfg := workloadConfig(t)

		cfg.ControlURL = endpoint
		if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
}

func TestDesiredDaemonSetRejectsMissingImage(t *testing.T) {
	for _, image := range []string{"", " \t"} {
		cfg := workloadConfig(t)

		cfg.DataplaneImage = image
		if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
			t.Fatalf("image %q: %v", image, err)
		}
	}
}

func TestWorkloadConfigIdentityAndNames(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"cluster":           func(c *Config) { c.Cluster = "invalid" },
		"missing cluster":   func(c *Config) { c.Cluster = "" },
		"namespace":         func(c *Config) { c.Namespace = "invalid.namespace" },
		"missing namespace": func(c *Config) { c.Namespace = "" },
		"zero port":         func(c *Config) { c.PeerPort = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := workloadConfig(t)
			mutate(&cfg)

			if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
				t.Fatalf("invalid workload config accepted: %v", err)
			}
		})
	}

	for _, name := range []string{"daemonset", "trust", "serviceaccount"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "../name", "Uppercase", strings.Repeat("a", 254)} {
				cfg := workloadConfig(t)
				fields := map[string]*string{
					"daemonset": &cfg.DaemonSetName,
					"trust":     &cfg.BootstrapTrustConfigMap, "serviceaccount": &cfg.DataplaneServiceAccount,
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

	cfg, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Namespace != "unbounded-system" || cfg.PeerPort != 8082 || cfg.DaemonSetName != "racer-dataplane" || cfg.DataplaneServiceAccount != "racer-dataplane" || cfg.BootstrapTrustConfigMap != "racer-bootstrap-trust" {
		t.Fatalf("unexpected workload defaults: %+v", cfg)
	}

	for key, value := range map[string]string{
		"POD_NAMESPACE": "custom", "RACER_PEER_PORT": "65535", "RACER_DAEMONSET_NAME": "custom.dataplane",
		"RACER_DATAPLANE_SERVICE_ACCOUNT": "custom.account",
		"RACER_BOOTSTRAP_TRUST_CONFIGMAP": "custom.trust",
	} {
		values[key] = value
	}

	for key := range values {
		t.Setenv(key, "invalid-process-value")
	}

	cfg, err = ConfigFromLookup(lookup)

	want := Config{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "custom", PeerPort: 65535,
		ControlURL: "https://controller:8443", DataplaneImage: "racer:test", DaemonSetName: "custom.dataplane",
		DataplaneServiceAccount: "custom.account", BootstrapTrustConfigMap: "custom.trust",
	}
	if err != nil || !reflect.DeepEqual(cfg, want) {
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

			if _, err := ConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
				t.Fatalf("%s=%q accepted: %v", key, invalid, err)
			}
		}

		values[key] = previous
	}

	for _, port := range []string{"0", "65536", "-1"} {
		values["RACER_PEER_PORT"] = port
		if _, err := ConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
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

	cfg, err := ConfigFromLookup(func(key string) (string, bool) {
		if value, ok := values[key]; ok {
			return value, true
		}

		switch key {
		case "RACER_PEER_PORT", "RACER_HOST_NETWORK", "RACER_POD_NETWORK_NODES", "RACER_DIAGNOSTICS_PORT", "RACER_DATAPLANE_SERVICE_ACCOUNT", "RACER_DAEMONSET_NAME", "RACER_BOOTSTRAP_TRUST_CONFIGMAP":
			return "", false
		default:
			t.Errorf("workload parser requested runtime setting %s", key)
			return "invalid", true
		}
	})
	if err != nil || !reflect.DeepEqual(cfg, want) {
		t.Fatalf("workload needs runtime configuration: %+v, %v", cfg, err)
	}

	if _, err := DesiredDaemonSet(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestWorkloadIgnoresLegacyKeyringSecret(t *testing.T) {
	want := workloadConfig(t)
	values := map[string]string{
		"RACER_CLUSTER_ID": string(want.Cluster), "POD_NAMESPACE": want.Namespace,
		"RACER_CONTROL_URL": want.ControlURL, "RACER_DATAPLANE_IMAGE": want.DataplaneImage,
		"RACER_CREDENTIALS_SECRET_NAME": "../obsolete",
	}

	cfg, err := ConfigFromLookup(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil || !reflect.DeepEqual(cfg, want) {
		t.Fatalf("legacy environment changed workload configuration: %+v, %v", cfg, err)
	}

	ds, err := DesiredDaemonSet(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Secret != nil || volume.Name == "keyring" {
			t.Fatal("legacy environment must not restore shared key mounts")
		}
	}
}

func TestWorkloadNetworkPortBounds(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:test",
		"RACER_HOST_NETWORK": "true", "RACER_PEER_PORT": "1024", "RACER_DIAGNOSTICS_PORT": "65535",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	for range 2 {
		cfg, err := ConfigFromLookup(lookup)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := DesiredDaemonSet(cfg); err != nil {
			t.Fatal(err)
		}

		cfg.DiagnosticsPort = cfg.PeerPort
		if _, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) {
			t.Fatal("direct builder must reject colliding ports")
		}

		cfg.DiagnosticsPort = 1023
		if _, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) {
			t.Fatal("direct builder must reject privileged diagnostics")
		}

		values["RACER_PEER_PORT"], values["RACER_DIAGNOSTICS_PORT"] = values["RACER_DIAGNOSTICS_PORT"], values["RACER_PEER_PORT"]
	}
}

func TestManagedNames(t *testing.T) {
	for _, tt := range []struct {
		name string
		want []string
	}{
		{DataplaneDaemonSetName, []string{DataplaneDaemonSetName, PodNetworkDaemonSetName}},
		{"custom-racer", []string{"custom-racer"}},
		{PodNetworkDaemonSetName, []string{PodNetworkDaemonSetName}},
		{"", []string{""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ManagedNames(tt.name)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ManagedNames(%q) = %v, want %v", tt.name, got, tt.want)
			}

			got[0] = "mutated"

			if !reflect.DeepEqual(ManagedNames(tt.name), tt.want) {
				t.Fatal("caller mutation changed managed names")
			}
		})
	}
}
