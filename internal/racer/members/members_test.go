// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// Workload configuration, projections, and placement.

func TestReadWorkloadIdentities(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	for _, name := range []string{DataplaneDaemonSetName, "custom"} {
		t.Run(name, func(t *testing.T) {
			live := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: name, UID: "current"}}
			terminating := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "terminating", Finalizers: []string{"test"}, DeletionTimestamp: ptr.To(metav1.Now())}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, terminating).Build()
			ids, err := ReadWorkloadIdentities(t.Context(), c, "racer", name)
			require.NoError(t, err)
			require.Equal(t, "racer", ids.Namespace)
			require.Equal(t, name, ids.Name)
			require.Equal(t, live.UID, ids.UID)

			require.NoError(t, c.Delete(t.Context(), live))
			ids, err = ReadWorkloadIdentities(t.Context(), c, "racer", name)
			require.NoError(t, err)
			require.Empty(t, ids.UID)
			require.Equal(t, name, ids.Name)
		})
	}

	boom := errors.New("read denied")
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return boom
	}}).Build()
	ids, err := ReadWorkloadIdentities(t.Context(), c, "racer", DataplaneDaemonSetName)
	require.ErrorIs(t, err, boom)
	require.Zero(t, ids)
}

func TestControllerPodIdentity(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", UID: "pod"}, Spec: corev1.PodSpec{ServiceAccountName: "controller"}}
	require.True(t, ControllerPod(pod, "racer", "controller"))

	for name, mutate := range map[string]func(*corev1.Pod){
		"namespace":       func(p *corev1.Pod) { p.Namespace = "other" },
		"UID":             func(p *corev1.Pod) { p.UID = "" },
		"terminating":     func(p *corev1.Pod) { p.DeletionTimestamp = ptr.To(metav1.Now()) },
		"service account": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" },
		"failed":          func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"succeeded":       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := pod.DeepCopy()
			mutate(invalid)
			require.False(t, ControllerPod(invalid, "racer", "controller"))
		})
	}
}

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

	assertWorkloadVolumes(t, pod, cfg)
	assertWorkloadMounts(t, pod.Containers[0])

	requirements := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if requirements[0].Key != wire.ExclusionLabel || requirements[0].Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Fatal("exclusion must test presence")
	}

	for _, env := range pod.Containers[0].Env {
		if env.ValueFrom != nil && (env.Name != "RACER_POD_IP" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}})) {
			t.Fatal("node identity must come from verified enrollment")
		}
	}

	if len(pod.Containers[0].Args) != 0 || len(pod.Containers[0].Command) != 0 {
		t.Fatal("workload must use the image entrypoint")
	}
}

func assertWorkloadVolumes(t *testing.T, pod corev1.PodSpec, cfg Config) {
	t.Helper()

	volumes := map[string]corev1.Volume{}

	for _, v := range pod.Volumes {
		assertNoSecretVolume(t, v)
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

	wantDevices := &corev1.HostPathVolumeSource{Path: "/dev", Type: ptr.To(corev1.HostPathDirectory)}
	if !reflect.DeepEqual(volumes["devices"].HostPath, wantDevices) {
		t.Fatal("block device discovery requires the existing host /dev directory")
	}
}

func assertWorkloadMounts(t *testing.T, container corev1.Container) {
	t.Helper()

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
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
		"devices":    {Name: "devices", MountPath: "/host/dev", ReadOnly: true},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Fatal("mounts must retain token, controller trust, private identity, slabs, sockets, RDMA devices and host devices only")
	}
}

func assertNoSecretVolume(t *testing.T, volume corev1.Volume) {
	t.Helper()

	if volume.Secret != nil {
		t.Fatal("dataplane must fetch shared keys over control HTTPS, not mount Secrets")
	}

	if volume.Projected == nil {
		return
	}

	for _, source := range volume.Projected.Sources {
		if source.Secret != nil {
			t.Fatal("dataplane must not project Secrets")
		}
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

			if len(pod.Volumes) != 7 {
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

			env := environmentValues(t, container)

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
				"RACER_DEVICE_DIRECTORY":      "/host/dev",
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

			assertEnvironmentMountPaths(t, container, env)
		})
	}
}

func assertWorkloadReadiness(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()

	rolling := ds.Spec.UpdateStrategy.RollingUpdate

	wantRolling := &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromInt32(0))}
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType || !reflect.DeepEqual(rolling, wantRolling) || ds.Spec.MinReadySeconds != 10 {
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

	assertDiagnosticsEnvironment(t, container, ports["diagnostics"])
}

func assertDiagnosticsEnvironment(t *testing.T, container corev1.Container, diagnosticsPort int32) {
	t.Helper()

	podIPSeen, diagnosticsSeen := false, false

	for _, env := range container.Env {
		switch env.Name {
		case "RACER_POD_IP":
			if podIPSeen || env.Value != "" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}) {
				t.Fatal("bind address must come from the downward API Pod IP")
			}

			podIPSeen = true
		case "RACER_DIAGNOSTICS_LISTEN":
			if !podIPSeen || diagnosticsSeen || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(diagnosticsPort)) {
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

func environmentValues(t *testing.T, container corev1.Container) map[string]string {
	t.Helper()

	env := map[string]string{}
	for _, value := range container.Env {
		if _, exists := env[value.Name]; exists {
			t.Fatalf("duplicate configuration: %s", value.Name)
		}

		env[value.Name] = value.Value
	}

	return env
}

func assertEnvironmentMountPaths(t *testing.T, container corev1.Container, env map[string]string) {
	t.Helper()

	mounts := map[string]string{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}

	for name, location := range map[string]string{
		"RACER_TRUST_BUNDLE":          path.Join(mounts["bootstrap"], "ca.crt"),
		"RACER_SERVICE_ACCOUNT_TOKEN": path.Join(mounts["token"], "token"),
		"RACER_IDENTITY_DIRECTORY":    path.Join(mounts["identity"], "private"),
		"RACER_SLAB_DIRECTORY":        mounts["slabs"],
		"RACER_DEVICE_DIRECTORY":      mounts["devices"],
	} {
		if env[name] != location {
			t.Errorf("%s does not match its mounted projection or storage", name)
		}
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

	assertInvalidLookupValues(t, values)

	for _, port := range []string{"0", "65536", "-1"} {
		values["RACER_PEER_PORT"] = port
		if _, err := ConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("port %q accepted: %v", port, err)
		}
	}
}

func assertInvalidLookupValues(t *testing.T, values map[string]string) {
	t.Helper()

	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
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
		{DataplaneDaemonSetName, []string{DataplaneDaemonSetName}},
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

// Observed membership, recovery, and catalog candidates.

const (
	nodeID  = "11111111-1111-4111-8111-111111111111"
	otherID = "22222222-2222-4222-8222-222222222222"
)

func observedInput() Input {
	return Input{
		Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: nodeID}}},
		PodsByNode: map[string][]corev1.Pod{"node": {{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "racer", Name: "pod", UID: "pod-uid",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "dataplane", UID: "workload-uid", Controller: ptr.To(true)}},
			},
			Spec:   corev1.PodSpec{NodeName: "node"},
			Status: corev1.PodStatus{PodIP: "192.0.2.1"},
		}}},
		Ownership: WorkloadIdentities{Namespace: "racer", Name: "dataplane", UID: "workload-uid"},
		PeerPort:  7443,
	}
}

func TestReconcileCandidateRequiresExplicitHistoryAdvance(t *testing.T) {
	input := observedInput()
	input.Nodes[0].Annotations = map[string]string{wire.SharesAnnotation: "8", wire.RDMANICsAnnotation: `[{"device":"nic","port":1,"rail":0,"numa_node":1}]`}
	accepted := History{}

	candidate, err := Reconcile(input, accepted)
	if err != nil || len(candidate.Diagnostics) != 0 || len(candidate.Members) != 1 || len(accepted) != 0 {
		t.Fatalf("candidate changed accepted history: %+v, %v, %v", candidate, accepted, err)
	}

	input.PodsByNode = nil
	input.Nodes[0].Annotations[wire.SharesAnnotation] = "invalid"

	unpublished, err := Reconcile(input, accepted)
	if err != nil || len(unpublished.Members) != 0 || len(unpublished.Diagnostics) != 2 {
		t.Fatalf("unpublished candidate survived a gap: %+v, %v", unpublished, err)
	}

	accepted = candidate.Members // Simulate a successful publication.

	retained, err := Reconcile(input, accepted)
	if err != nil || !reflect.DeepEqual(retained.Members, accepted) {
		t.Fatalf("accepted member did not survive gap: %+v, %v", retained, err)
	}

	retained.Members[nodeID].RDMANICs[0].Device = "changed"
	*retained.Members[nodeID].RDMANICs[0].NUMANode = 9
	delete(retained.Members, nodeID)

	if accepted[nodeID].RDMANICs[0].Device != "nic" || *accepted[nodeID].RDMANICs[0].NUMANode != 1 {
		t.Fatal("result aliases nested accepted history")
	}
}

func TestReconcileRecoveryIsUIDBoundAndSiteIsCurrent(t *testing.T) {
	input := observedInput()
	input.Nodes[0].Labels = map[string]string{machinav1.MachineSiteLabelKey: "old-site"}

	initial, err := Reconcile(input, nil)
	if err != nil {
		t.Fatal(err)
	}

	saved, err := json.Marshal(initial.Members[nodeID])
	if err != nil {
		t.Fatal(err)
	}

	input.PodsByNode = nil
	input.Nodes[0].Annotations = map[string]string{AdmittedMemberAnnotation: string(saved), wire.SharesAnnotation: "invalid"}
	input.Nodes[0].Labels = nil

	recovered, err := Reconcile(input, nil)
	if err != nil || len(recovered.Members) != 1 || recovered.Members[nodeID].Site != "" || recovered.Members[nodeID].PeerEndpoint != "192.0.2.1:7443" {
		t.Fatalf("recovery retained stale site or lost endpoint: %+v, %v", recovered, err)
	}

	input.Nodes[0].UID = otherID

	recreated, err := Reconcile(input, nil)
	if err != nil || len(recreated.Members) != 0 {
		t.Fatalf("recreated Node inherited history: %+v, %v", recreated, err)
	}

	input.Nodes[0].UID = nodeID
	input.Nodes[0].Annotations[AdmittedMemberAnnotation] = "malformed"

	invalid, err := Reconcile(input, nil)
	if err != nil || len(invalid.Members) != 0 {
		t.Fatalf("malformed recovery admitted: %+v, %v", invalid, err)
	}
}

func TestReconcileRejectsWholeInputAndOrdersDiagnostics(t *testing.T) {
	input := observedInput()
	input.Nodes = append(input.Nodes, input.Nodes[0])

	result, err := Reconcile(input, nil)
	if !errors.Is(err, wire.InvalidRequest) || result.Members != nil || result.Diagnostics != nil {
		t.Fatalf("partial result escaped: %+v, %v", result, err)
	}

	input.Nodes[1] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: otherID}}
	input.PodsByNode = nil

	first, err := Reconcile(input, nil)
	if err != nil || len(first.Diagnostics) != 2 || first.Diagnostics[0].Object != "node" || first.Diagnostics[1].Object != "other" {
		t.Fatalf("unexpected diagnostics: %+v, %v", first, err)
	}

	slices.Reverse(input.Nodes)

	second, err := Reconcile(input, nil)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("order changed result: %+v, %v", second, err)
	}

	input.PeerPort = 0
	if _, err := Reconcile(input, nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("zero port admitted: %v", err)
	}
}

func TestObservedOwnershipAndEndpoint(t *testing.T) {
	input := observedInput()

	pod := input.PodsByNode["node"][0]
	if !input.Ownership.Owns(&pod) || input.Ownership.Owns(nil) {
		t.Fatal("observed ownership mismatch")
	}

	for _, identity := range []WorkloadIdentities{{Name: "dataplane"}, {Name: "other", UID: "workload-uid"}, {Name: "dataplane", UID: "recreated"}} {
		ownership := identity

		ownership.Namespace = input.Ownership.Namespace
		if ownership.Owns(&pod) {
			t.Fatalf("unobserved identity admitted: %+v", identity)
		}
	}
	// A newer Pod from another workload is not eligible.
	newer := pod.DeepCopy()
	newer.UID = "z"
	newer.OwnerReferences[0].Name, newer.OwnerReferences[0].UID = "podnet", "podnet-uid"
	newer.Status.PodIP = "2001:db8::1"

	endpoint, err := SelectEndpoint([]corev1.Pod{*newer, pod}, input.Ownership, "node", 7443)
	if err != nil || endpoint != "192.0.2.1:7443" {
		t.Fatalf("mixed workload endpoint: %q, %v", endpoint, err)
	}

	newer.Status.Phase = corev1.PodSucceeded

	endpoint, err = SelectEndpoint([]corev1.Pod{*newer, pod}, input.Ownership, "node", 7443)
	if err != nil || endpoint != "192.0.2.1:7443" {
		t.Fatalf("terminal endpoint admitted: %q, %v", endpoint, err)
	}
}

func TestCatalogOrderingAndWholeCandidateValidation(t *testing.T) {
	caches := []racerv1.ClusterCache{
		{ObjectMeta: metav1.ObjectMeta{Name: "cache-b", UID: types.UID(otherID)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cache-a", UID: types.UID(nodeID)}},
	}

	catalog, err := BuildCatalog(caches)
	if err != nil || len(catalog) != 2 || catalog[0].ID != nodeID || catalog[0].ClientSocket != "/run/racer/cache-a/client/socket" || caches[0].Name != "cache-b" {
		t.Fatalf("catalog order or input mutation: %+v, %v", catalog, err)
	}

	caches[1].Name = "../invalid"

	catalog, err = BuildCatalog(caches)
	if !errors.Is(err, wire.InvalidRequest) || catalog != nil {
		t.Fatalf("partial catalog escaped: %+v, %v", catalog, err)
	}
}

func TestParseAnnotations(t *testing.T) {
	nicJSON := `[{"device":"nic","port":1,"rail":0}]`

	nics := []wire.RDMANIC{{Device: "nic", Port: 1, Rail: 0}}
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		shares      uint32
		nics        []wire.RDMANIC
		errorField  string
	}{
		{name: "defaults", shares: wire.DefaultShares},
		{name: "empty enrolled shares", annotations: map[string]string{EnrolledSharesAnnotation: ""}, shares: wire.DefaultShares},
		{name: "enrolled shares", annotations: map[string]string{EnrolledSharesAnnotation: "8"}, shares: 8},
		{name: "explicit overrides invalid enrolled", annotations: map[string]string{EnrolledSharesAnnotation: "invalid", wire.SharesAnnotation: "9"}, shares: 9},
		{name: "maximum shares", annotations: map[string]string{wire.SharesAnnotation: "4294967295"}, shares: 4294967295},
		{name: "zero enrolled", annotations: map[string]string{EnrolledSharesAnnotation: "0"}, errorField: "bare"},
		{name: "invalid enrolled", annotations: map[string]string{EnrolledSharesAnnotation: "invalid"}, errorField: "bare"},
		{name: "zero explicit", annotations: map[string]string{wire.SharesAnnotation: "0"}, errorField: wire.SharesAnnotation},
		{name: "empty explicit", annotations: map[string]string{wire.SharesAnnotation: ""}, errorField: wire.SharesAnnotation},
		{name: "plus explicit", annotations: map[string]string{wire.SharesAnnotation: "+1"}, errorField: wire.SharesAnnotation},
		{name: "negative explicit", annotations: map[string]string{wire.SharesAnnotation: "-1"}, errorField: wire.SharesAnnotation},
		{name: "overflow explicit", annotations: map[string]string{wire.SharesAnnotation: "4294967296"}, errorField: wire.SharesAnnotation},
		{name: "enrolled NICs", annotations: map[string]string{EnrolledRDMANICsAnnotation: nicJSON}, shares: wire.DefaultShares, nics: nics},
		{name: "explicit NICs", annotations: map[string]string{wire.RDMANICsAnnotation: nicJSON}, shares: wire.DefaultShares, nics: nics},
		{name: "explicit clears enrolled NICs", annotations: map[string]string{wire.RDMANICsAnnotation: "[]", EnrolledRDMANICsAnnotation: nicJSON}, shares: wire.DefaultShares},
		{name: "invalid enrolled NICs", annotations: map[string]string{EnrolledRDMANICsAnnotation: "invalid"}, errorField: EnrolledRDMANICsAnnotation},
		{name: "invalid explicit NICs", annotations: map[string]string{wire.RDMANICsAnnotation: "", EnrolledRDMANICsAnnotation: nicJSON}, errorField: wire.RDMANICsAnnotation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			before := node.DeepCopy()

			got, err := ParseAnnotations(node)
			if !reflect.DeepEqual(node, before) {
				t.Fatal("annotations mutated")
			}

			if tt.errorField != "" {
				assertAnnotationError(t, got, err, tt.errorField)
				return
			}

			if err != nil || got.Shares != tt.shares || !slices.EqualFunc(got.RDMANICs, tt.nics, func(a, b wire.RDMANIC) bool { return reflect.DeepEqual(a, b) }) {
				t.Fatalf("attributes = %+v, %v", got, err)
			}
		})
	}

	got, err := ParseAnnotations(nil)
	assertAnnotationError(t, got, err, "bare")
}

func assertAnnotationError(t *testing.T, got MemberAttributes, err error, field string) {
	t.Helper()

	if !errors.Is(err, wire.InvalidRequest) || !reflect.DeepEqual(got, MemberAttributes{}) {
		t.Fatalf("partial attributes or wrong error: %+v, %v", got, err)
	}

	if field == "bare" {
		if err != wire.InvalidRequest {
			t.Fatalf("wrapped enrolled error: %v", err)
		}
	} else if !strings.HasPrefix(err.Error(), field+": ") {
		t.Fatalf("missing field %s: %v", field, err)
	}
}

func TestEndpointEligibility(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"wrong node":      func(p *corev1.Pod) { p.Spec.NodeName = "other" },
		"terminating":     func(p *corev1.Pod) { p.DeletionTimestamp = ptr.To(metav1.Now()) },
		"missing UID":     func(p *corev1.Pod) { p.UID = "" },
		"failed":          func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"succeeded":       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"missing IP":      func(p *corev1.Pod) { p.Status.PodIP = "" },
		"invalid IP":      func(p *corev1.Pod) { p.Status.PodIP = "hostname" },
		"zoned IP":        func(p *corev1.Pod) { p.Status.PodIP = "fe80::1%eth0" },
		"wrong namespace": func(p *corev1.Pod) { p.Namespace = "other" },
		"no owner":        func(p *corev1.Pod) { p.OwnerReferences = nil },
		"not controller":  func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
		"wrong API":       func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "apps/v2" },
		"wrong kind":      func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Deployment" },
		"empty owner UID": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			input := observedInput()
			pod := input.PodsByNode["node"][0]
			mutate(&pod)

			endpoint, err := SelectEndpoint([]corev1.Pod{pod}, input.Ownership, "node", 7443)
			if !errors.Is(err, wire.Unavailable) || endpoint != "" {
				t.Fatalf("ineligible endpoint: %q, %v", endpoint, err)
			}
		})
	}
}

func TestEndpointSelectionOrderAndArguments(t *testing.T) {
	input := observedInput()
	older := input.PodsByNode["node"][0]
	newer := *older.DeepCopy()
	newer.UID = "a"
	newer.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	newer.Status.PodIP = "192.0.2.2"

	newer.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	for _, pods := range [][]corev1.Pod{{older, newer}, {newer, older}} {
		endpoint, err := SelectEndpoint(pods, input.Ownership, "node", 7443)
		if err != nil || endpoint != "192.0.2.2:7443" {
			t.Fatalf("creation time or readiness selection: %q, %v", endpoint, err)
		}
	}

	for _, tt := range []struct {
		node string
		port uint16
	}{{"", 7443}, {"node", 0}} {
		endpoint, err := SelectEndpoint(nil, input.Ownership, tt.node, tt.port)
		if !errors.Is(err, wire.InvalidRequest) || endpoint != "" {
			t.Fatalf("invalid arguments: %q, %v", endpoint, err)
		}
	}
}

func TestReconcileIdentityValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"invalid UID":    func(i *Input) { i.Nodes[0].UID = "invalid" },
		"empty name":     func(i *Input) { i.Nodes[0].Name = "" },
		"duplicate name": func(i *Input) { n := *i.Nodes[0].DeepCopy(); n.UID = otherID; i.Nodes = append(i.Nodes, n) },
		"excluded invalid identity": func(i *Input) {
			i.Nodes[0].UID = "invalid"
			i.Nodes[0].Labels = map[string]string{wire.ExclusionLabel: ""}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := observedInput()
			mutate(&input)

			got, err := Reconcile(input, nil)
			if !errors.Is(err, wire.InvalidRequest) || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("invalid identity: %+v, %v", got, err)
			}
		})
	}
}

func TestReconcileRemovalAndLegacyDiagnostics(t *testing.T) {
	input := observedInput()

	initial, err := Reconcile(input, nil)
	if err != nil {
		t.Fatal(err)
	}

	input.Nodes[0].Annotations = map[string]string{"racer.unbounded-cloud.io/rails": "ignored", "racer.unbounded-cloud.io/aligned-rails": "ignored"}
	result, err := Reconcile(input, initial.Members)

	wantDiagnostics := []Diagnostic{}
	if err != nil || !reflect.DeepEqual(result.Diagnostics, wantDiagnostics) || !reflect.DeepEqual(result.Members, initial.Members) {
		t.Fatalf("legacy diagnostics: %+v, %v", result, err)
	}

	for _, excluded := range []bool{true, false} {
		if excluded {
			input.Nodes[0].Labels = map[string]string{wire.ExclusionLabel: "false"}
		} else {
			input.Nodes = nil
		}

		result, err := Reconcile(input, initial.Members)
		if err != nil || len(result.Members) != 0 || len(result.Diagnostics) != 0 {
			t.Fatalf("removed node retained: %+v, %v", result, err)
		}
	}
}

func TestReconcileMemberLimit(t *testing.T) {
	input := Input{PeerPort: 7443}
	accepted := History{}

	for i := range wire.MaxMembers + 1 {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		input.Nodes = append(input.Nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), UID: types.UID(id)}})
		accepted[wire.NodeID(id)] = wire.Member{Node: wire.NodeID(id), Shares: 1, PeerEndpoint: "192.0.2.1:7443"}
	}

	got, err := Reconcile(input, accepted)
	if !errors.Is(err, wire.TooLarge) || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("oversized membership: %d, %v", len(got.Members), err)
	}

	input.Nodes = input.Nodes[:wire.MaxMembers]

	got, err = Reconcile(input, accepted)
	if err != nil || len(got.Members) != wire.MaxMembers {
		t.Fatalf("boundary membership: %d, %v", len(got.Members), err)
	}
}

func TestCatalogRejectsInvalidIdentities(t *testing.T) {
	valid := racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: nodeID}}
	for _, identity := range []metav1.ObjectMeta{{Name: "other", UID: "invalid"}, {Name: "other", UID: nodeID}, {Name: "cache", UID: otherID}} {
		invalid := valid
		invalid.ObjectMeta = identity

		got, err := BuildCatalog([]racerv1.ClusterCache{valid, invalid})
		if !errors.Is(err, wire.InvalidRequest) || got != nil {
			t.Fatalf("invalid catalog identity: %+v, %v", got, err)
		}
	}
}

func TestConfigValidationErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"exceptions without host networking", func(c *Config) { c.PodNetworkNodes = []string{"node"} }, "pod network exceptions require host networking and the fixed dataplane name"},
		{"exceptions with custom name", func(c *Config) {
			c.HostNetwork = true
			c.DaemonSetName = "custom"
			c.PodNetworkNodes = []string{"node"}
		}, "pod network exceptions require host networking and the fixed dataplane name"},
		{"duplicate nodes", func(c *Config) { c.HostNetwork = true; c.PodNetworkNodes = []string{"node", "node"} }, "pod network nodes must be unique valid node names"},
		{"invalid node", func(c *Config) { c.HostNetwork = true; c.PodNetworkNodes = []string{"Uppercase"} }, "pod network nodes must be unique valid node names"},
		{"privileged peer port", func(c *Config) { c.PeerPort = 1023 }, "cluster, namespace, or peer port"},
		{"long selector label", func(c *Config) { c.DaemonSetName = strings.Repeat("a", 64) }, "DaemonSet name must fit a label value"},
		{"escaped path", func(c *Config) { c.ControlURL = "https://host/%2f" }, "workload endpoint or image"},
		{"query", func(c *Config) { c.ControlURL = "https://host/?x=y" }, "workload endpoint or image"},
		{"malformed URL", func(c *Config) { c.ControlURL = "https://host:%" }, "workload endpoint or image"},
		{"missing host", func(c *Config) { c.ControlURL = "https:///" }, "workload endpoint or image"},
		{"zero endpoint port", func(c *Config) { c.ControlURL = "https://host:0" }, "workload endpoint port"},
		{"overflow endpoint port", func(c *Config) { c.ControlURL = "https://host:65536" }, "workload endpoint port"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workloadConfig(t)
			tt.mutate(&cfg)
			err := cfg.Validate()

			want := tt.message + ": " + wire.InvalidRequest.Error()
			if !errors.Is(err, wire.InvalidRequest) || err.Error() != want {
				t.Fatalf("error = %v, want %s", err, want)
			}

			sets, err := DesiredDaemonSets(cfg)
			if !errors.Is(err, wire.InvalidRequest) || sets != nil {
				t.Fatalf("invalid planner config: %+v, %v", sets, err)
			}
		})
	}
}

func TestConfigLookupNetworkSettings(t *testing.T) {
	for _, tt := range []struct {
		key, value string
		valid      bool
	}{
		{"RACER_HOST_NETWORK", "true", true},
		{"RACER_HOST_NETWORK", "false", true},
		{"RACER_HOST_NETWORK", "TRUE", false},
		{"RACER_HOST_NETWORK", "", false},
		{"RACER_DIAGNOSTICS_PORT", "1024", true},
		{"RACER_DIAGNOSTICS_PORT", "65535", true},
		{"RACER_DIAGNOSTICS_PORT", "1023", false},
		{"RACER_DIAGNOSTICS_PORT", "65536", false},
		{"RACER_DIAGNOSTICS_PORT", "", false},
		{"RACER_POD_NETWORK_NODES", "[]", true},
		{"RACER_POD_NETWORK_NODES", `["node-b","node-a"]`, true},
		{"RACER_POD_NETWORK_NODES", "null", false},
		{"RACER_POD_NETWORK_NODES", "{}", false},
		{"RACER_POD_NETWORK_NODES", "[1]", false},
		{"RACER_POD_NETWORK_NODES", "", false},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			values := map[string]string{"RACER_CLUSTER_ID": nodeID, "RACER_CONTROL_URL": "https://controller/", "RACER_DATAPLANE_IMAGE": "racer:test", "RACER_HOST_NETWORK": "true"}
			values[tt.key] = tt.value

			cfg, err := ConfigFromLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}

				if _, err := DesiredDaemonSets(cfg); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, wire.InvalidRequest) || !reflect.DeepEqual(cfg, Config{}) {
				t.Fatalf("parse failure = %+v, %v", cfg, err)
			}
		})
	}
}

func TestDesiredDaemonSets(t *testing.T) {
	for _, hostNetwork := range []bool{false, true} {
		cfg := workloadConfig(t)
		cfg.HostNetwork = hostNetwork

		one, err := DesiredDaemonSet(cfg)
		if err != nil {
			t.Fatal(err)
		}

		sets, err := DesiredDaemonSets(cfg)
		if err != nil || !reflect.DeepEqual(sets, []*appsv1.DaemonSet{one}) {
			t.Fatalf("single-workload plan differs: %+v, %v", sets, err)
		}
	}

	cfg := workloadConfig(t)
	cfg.HostNetwork = true

	cfg.PodNetworkNodes = []string{"node-b", "node-a"}
	if ds, err := DesiredDaemonSet(cfg); !errors.Is(err, wire.InvalidRequest) || ds != nil {
		t.Fatalf("single builder accepted mixed mode: %+v, %v", ds, err)
	}

	sets, err := DesiredDaemonSets(cfg)
	if err != nil || len(sets) != 2 {
		t.Fatalf("mixed plan: %+v, %v", sets, err)
	}

	if !slices.Equal(cfg.PodNetworkNodes, []string{"node-b", "node-a"}) {
		t.Fatal("planner reordered caller nodes")
	}

	assertMixedPlacement(t, sets)

	sets[1].Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[1].Values[0] = "mutated"
	if sets[1].Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[1].MatchExpressions[1].Values[0] != "linux" {
		t.Fatal("placement terms alias each other")
	}
}

func assertMixedPlacement(t *testing.T, sets []*appsv1.DaemonSet) {
	t.Helper()

	for i, ds := range sets {
		name := []string{DataplaneDaemonSetName, PodNetworkDaemonSetName}[i]

		pod := ds.Spec.Template.Spec
		if ds.Name != name || pod.HostNetwork != (i == 0) {
			t.Fatalf("workload identity or network: %+v", ds)
		}

		wantDNS := []corev1.DNSPolicy{corev1.DNSClusterFirstWithHostNet, corev1.DNSClusterFirst}[i]
		if pod.DNSPolicy != wantDNS {
			t.Fatalf("DNS = %s, want %s", pod.DNSPolicy, wantDNS)
		}

		labels := map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/instance": name}
		if !reflect.DeepEqual(ds.Labels, labels) || !reflect.DeepEqual(ds.Spec.Selector.MatchLabels, labels) || !reflect.DeepEqual(ds.Spec.Template.Labels, labels) {
			t.Fatalf("overlapping selectors: %+v", ds.Spec.Selector)
		}

		terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		assertPlacementTerms(t, terms, i == 0)
	}
}

func assertPlacementTerms(t *testing.T, terms []corev1.NodeSelectorTerm, host bool) {
	t.Helper()

	base := []corev1.NodeSelectorRequirement{
		{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist},
		{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
	}

	want := []corev1.NodeSelectorTerm{}
	if host {
		want = append(want, corev1.NodeSelectorTerm{MatchExpressions: base, MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-a"}},
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-b"}},
		}})
	} else {
		for _, node := range []string{"node-a", "node-b"} {
			want = append(want, corev1.NodeSelectorTerm{MatchExpressions: base, MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}})
		}
	}

	if !reflect.DeepEqual(terms, want) {
		t.Fatalf("placement = %+v, want %+v", terms, want)
	}
}
