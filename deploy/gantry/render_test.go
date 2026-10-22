// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDaemonSetMountsContainerdRuntimeDirectory(t *testing.T) {
	t.Parallel()

	outputDir := renderTemplates(t)

	raw, err := os.ReadFile(filepath.Join(outputDir, "daemonset.yaml"))
	if err != nil {
		t.Fatalf("read rendered daemonset: %v", err)
	}

	var daemonSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string `yaml:"name"`
						VolumeMounts []struct {
							Name             string `yaml:"name"`
							MountPath        string `yaml:"mountPath"`
							SubPath          string `yaml:"subPath"`
							MountPropagation string `yaml:"mountPropagation"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name     string `yaml:"name"`
						HostPath struct {
							Path string `yaml:"path"`
							Type string `yaml:"type"`
						} `yaml:"hostPath"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &daemonSet); err != nil {
		t.Fatalf("unmarshal rendered daemonset: %v", err)
	}

	var mountPath, subPath, mountPropagation string

	for _, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name != "gantry" {
			continue
		}

		for _, mount := range container.VolumeMounts {
			if mount.Name == "containerd-runtime" {
				mountPath = mount.MountPath
				subPath = mount.SubPath
				mountPropagation = mount.MountPropagation
			}
		}
	}

	if mountPath != "/run/containerd" {
		t.Fatalf("containerd runtime mountPath = %q, want /run/containerd", mountPath)
	}

	if subPath != "" {
		t.Fatalf("containerd runtime subPath = %q, want empty so socket replacement remains visible", subPath)
	}

	if mountPropagation != "" {
		t.Fatalf("containerd runtime mountPropagation = %q, want default None", mountPropagation)
	}

	var hostPath, hostPathType string

	for _, volume := range daemonSet.Spec.Template.Spec.Volumes {
		if volume.Name == "containerd-runtime" {
			hostPath = volume.HostPath.Path
			hostPathType = volume.HostPath.Type
		}
	}

	if hostPath != "/run/containerd" || hostPathType != "Directory" {
		t.Fatalf("containerd runtime hostPath = %q type %q, want /run/containerd type Directory", hostPath, hostPathType)
	}
}

func TestArtifactStreamingDefaultsRenderDisabled(t *testing.T) {
	t.Parallel()

	outputDir := renderTemplates(t)

	raw, err := os.ReadFile(filepath.Join(outputDir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered configmap: %v", err)
	}

	var configMap struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &configMap); err != nil {
		t.Fatalf("unmarshal rendered configmap: %v", err)
	}

	config := configMap.Data["config.yaml"]
	for _, expected := range []string{
		"artifact_streaming_enabled: false",
		".data.mcr.microsoft.com",
		".blob.core.windows.net",
	} {
		if !strings.Contains(config, expected) {
			t.Errorf("rendered config missing %q:\n%s", expected, config)
		}
	}
}

func TestOverlayBDConfiguratorIsOptIn(t *testing.T) {
	t.Parallel()

	outputDir := renderStandaloneTemplates(t)
	if _, err := os.Stat(filepath.Join(outputDir, "overlaybd-config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("overlaybd-config.yaml exists by default; err=%v", err)
	}
}

func TestOverlayBDConfiguratorEnabled(t *testing.T) {
	t.Parallel()

	outputDir := renderChart(t, false,
		"--set", "overlaybdConfig.enabled=true",
		"--set", "gantry.artifactStreaming.enabled=true",
		"--set-string", "overlaybdConfig.image.reference=gantry-node-config:test",
		"--set-string", "overlaybdConfig.nodeSelector.kubernetes\\.azure\\.com/host-os=AzureLinux",
	)

	raw, err := os.ReadFile(filepath.Join(outputDir, "overlaybd-config.yaml"))
	if err != nil {
		t.Fatalf("read rendered OverlayBD configurator: %v", err)
	}

	var daemonSet struct {
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"selector"`
			Template struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
				Spec struct {
					HostPID      bool              `yaml:"hostPID"`
					NodeSelector map[string]string `yaml:"nodeSelector"`
					Containers   []struct {
						Name            string   `yaml:"name"`
						Image           string   `yaml:"image"`
						Args            []string `yaml:"args"`
						SecurityContext struct {
							Privileged bool `yaml:"privileged"`
						} `yaml:"securityContext"`
						VolumeMounts []struct {
							Name             string `yaml:"name"`
							MountPath        string `yaml:"mountPath"`
							MountPropagation string `yaml:"mountPropagation"`
							ReadOnly         bool   `yaml:"readOnly"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name     string `yaml:"name"`
						HostPath struct {
							Path string `yaml:"path"`
							Type string `yaml:"type"`
						} `yaml:"hostPath"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &daemonSet); err != nil {
		t.Fatalf("unmarshal OverlayBD configurator: %v", err)
	}

	if !daemonSet.Spec.Template.Spec.HostPID {
		t.Error("hostPID = false, want true")
	}

	for key, value := range daemonSet.Spec.Selector.MatchLabels {
		if daemonSet.Spec.Template.Metadata.Labels[key] != value {
			t.Errorf("selector label %s=%q does not match pod label %q", key, value, daemonSet.Spec.Template.Metadata.Labels[key])
		}
	}

	if got := daemonSet.Spec.Template.Spec.NodeSelector["kubernetes.azure.com/host-os"]; got != "AzureLinux" {
		t.Errorf("node selector = %q, want AzureLinux", got)
	}

	if len(daemonSet.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("unexpected configurator container: %+v", daemonSet.Spec.Template.Spec.Containers)
	}

	containers := daemonSet.Spec.Template.Spec.Containers
	if containers[0].Name != "snapshotter" || !containers[0].SecurityContext.Privileged {
		t.Fatalf("unexpected snapshotter container: %+v", containers[0])
	}

	if !slices.Contains(containers[0].Args, "--fallback-snapshotter=overlayfs") {
		t.Fatalf("snapshotter args = %v, want overlayfs fallback", containers[0].Args)
	}

	if containers[1].Name != "configure" || containers[1].Image != "gantry-node-config:test" || !containers[1].SecurityContext.Privileged {
		t.Fatalf("unexpected configurator container: %+v", containers[1])
	}

	foundBidirectional := false

	for _, volumeMount := range containers[0].VolumeMounts {
		if volumeMount.Name == "snapshotter-state" && volumeMount.MountPropagation == "Bidirectional" {
			foundBidirectional = true
		}

		if volumeMount.Name == "containerd-runtime" {
			t.Fatal("snapshotter must not access the host snapshot service")
		}
	}

	if !foundBidirectional {
		t.Fatal("snapshotter state mount is not bidirectionally propagated")
	}

	for _, volume := range daemonSet.Spec.Template.Spec.Volumes {
		if volume.Name == "containerd-runtime" {
			t.Fatal("snapshotter must not mount the host containerd runtime directory")
		}
	}
}

// TestInitContainerPreparesIdentityDirectory pins how the agent gets its libp2p
// identity hostPath: the gantry-init container runs gantry init from the
// agent's own image, as root with only the capabilities it needs, and the agent
// itself runs as UID 65532 with none.
func TestInitContainerPreparesIdentityDirectory(t *testing.T) {
	t.Parallel()

	type securityContext struct {
		RunAsNonRoot             *bool  `yaml:"runAsNonRoot"`
		RunAsUser                *int64 `yaml:"runAsUser"`
		AllowPrivilegeEscalation *bool  `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   *bool  `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Add  []string `yaml:"add"`
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	}

	type container struct {
		Name            string          `yaml:"name"`
		Image           string          `yaml:"image"`
		ImagePullPolicy string          `yaml:"imagePullPolicy"`
		Args            []string        `yaml:"args"`
		SecurityContext securityContext `yaml:"securityContext"`
		VolumeMounts    []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
		} `yaml:"volumeMounts"`
	}

	for profile, outputDir := range map[string]string{
		"operator":   renderTemplates(t),
		"standalone": renderStandaloneTemplates(t),
	} {
		raw, err := os.ReadFile(filepath.Join(outputDir, "daemonset.yaml"))
		if err != nil {
			t.Fatalf("%s: read rendered daemonset: %v", profile, err)
		}

		var daemonSet struct {
			Spec struct {
				Template struct {
					Spec struct {
						InitContainers []container `yaml:"initContainers"`
						Containers     []container `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &daemonSet); err != nil {
			t.Fatalf("%s: unmarshal rendered daemonset: %v", profile, err)
		}

		initContainers := daemonSet.Spec.Template.Spec.InitContainers
		if len(initContainers) != 1 || initContainers[0].Name != "gantry-init" {
			t.Fatalf("%s: init containers = %+v, want only gantry-init", profile, initContainers)
		}

		initContainer := initContainers[0]

		var agent *container

		for i := range daemonSet.Spec.Template.Spec.Containers {
			if daemonSet.Spec.Template.Spec.Containers[i].Name == "gantry" {
				agent = &daemonSet.Spec.Template.Spec.Containers[i]
			}
		}

		if agent == nil {
			t.Fatalf("%s: rendered daemonset has no gantry container", profile)
		}

		if initContainer.Image != agent.Image || initContainer.ImagePullPolicy != agent.ImagePullPolicy {
			t.Fatalf("%s: gantry-init runs %s (%s), want the agent's %s (%s)",
				profile, initContainer.Image, initContainer.ImagePullPolicy, agent.Image, agent.ImagePullPolicy)
		}

		if !reflect.DeepEqual(initContainer.Args, []string{"init"}) {
			t.Fatalf("%s: gantry-init args = %q, want [init] after the image's gantry entrypoint", profile, initContainer.Args)
		}

		mounted := false

		for _, mount := range initContainer.VolumeMounts {
			if mount.Name == "libp2p" && mount.MountPath == "/var/lib/gantry/libp2p" {
				mounted = true
			}
		}

		if !mounted {
			t.Fatalf("%s: gantry-init does not mount the libp2p volume at /var/lib/gantry/libp2p", profile)
		}

		for _, want := range []struct {
			name     string
			security securityContext
			uid      int64
			add      []string
		}{
			{name: "gantry-init", security: initContainer.SecurityContext, uid: 0, add: []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"}},
			{name: "gantry", security: agent.SecurityContext, uid: 65532},
		} {
			security := want.security

			if security.RunAsUser == nil || *security.RunAsUser != want.uid || security.RunAsNonRoot == nil || *security.RunAsNonRoot != (want.uid != 0) {
				t.Fatalf("%s: %s must run as UID %d with runAsNonRoot %t", profile, want.name, want.uid, want.uid != 0)
			}

			if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
				t.Fatalf("%s: %s must disallow privilege escalation", profile, want.name)
			}

			if security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
				t.Fatalf("%s: %s root filesystem must stay read-only", profile, want.name)
			}

			if !reflect.DeepEqual(security.Capabilities.Drop, []string{"ALL"}) {
				t.Fatalf("%s: %s drops capabilities %v, want [ALL]", profile, want.name, security.Capabilities.Drop)
			}

			if !reflect.DeepEqual(security.Capabilities.Add, want.add) {
				t.Fatalf("%s: %s adds capabilities %v, want %v", profile, want.name, security.Capabilities.Add, want.add)
			}
		}
	}
}

func TestRendersFixedChairLeaseSet(t *testing.T) {
	t.Parallel()

	outputDir := renderTemplates(t)

	raw, err := os.ReadFile(filepath.Join(outputDir, "rendezvous-leases.yaml"))
	if err != nil {
		t.Fatalf("read rendered chairs: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	seen := map[string]bool{}

	for {
		var object struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode chair manifest: %v", err)
		}

		if object.Kind == "" {
			continue
		}

		if object.Kind != "Lease" {
			t.Fatalf("rendered chair kind = %q, want Lease", object.Kind)
		}

		if object.Metadata.Annotations["helm.sh/resource-policy"] != "keep" {
			t.Fatalf("chair Lease %s is not retained across count reductions", object.Metadata.Name)
		}

		seen[object.Metadata.Name] = true
	}

	if len(seen) != 64 {
		t.Fatalf("chair Lease count = %d, want 64", len(seen))
	}

	for index := range 64 {
		name := fmt.Sprintf("gantry-chair-%02d", index)
		if !seen[name] {
			t.Fatalf("missing chair Lease %s", name)
		}
	}
}

func TestRendersOptionalMetricsService(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat(filepath.Join(renderChart(t, false), "metrics-service.yaml")); !os.IsNotExist(err) {
		t.Fatalf("metrics Service rendered by default (stat err: %v)", err)
	}

	outputDir := renderChart(t, false,
		"--set", "metricsService.enabled=true",
		"--set-string", "metricsService.annotations.prometheus\\.io/scrape=true",
	)

	raw, err := os.ReadFile(filepath.Join(outputDir, "metrics-service.yaml"))
	if err != nil {
		t.Fatalf("read rendered metrics Service: %v", err)
	}

	var service struct {
		Metadata struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			ClusterIP                string            `yaml:"clusterIP"`
			PublishNotReadyAddresses bool              `yaml:"publishNotReadyAddresses"`
			Selector                 map[string]string `yaml:"selector"`
			Ports                    []struct {
				Port       int    `yaml:"port"`
				TargetPort string `yaml:"targetPort"`
			} `yaml:"ports"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &service); err != nil {
		t.Fatalf("decode metrics Service: %v", err)
	}

	if service.Metadata.Name != "gantry-metrics" {
		t.Fatalf("metrics Service name = %q", service.Metadata.Name)
	}

	if service.Metadata.Annotations["prometheus.io/scrape"] != "true" {
		t.Fatalf("metrics Service annotations = %v", service.Metadata.Annotations)
	}

	if service.Spec.ClusterIP != "None" || !service.Spec.PublishNotReadyAddresses {
		t.Fatal("metrics Service must be headless and publish NotReady addresses")
	}

	if service.Spec.Selector["app.kubernetes.io/name"] != "gantry" || service.Spec.Selector["app.kubernetes.io/component"] != "agent" {
		t.Fatalf("metrics Service selector = %v", service.Spec.Selector)
	}

	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 9095 || service.Spec.Ports[0].TargetPort != "metrics" {
		t.Fatalf("metrics Service ports = %+v", service.Spec.Ports)
	}
}

func TestRendersConfiguredPeerTransferSettings(t *testing.T) {
	t.Parallel()

	defaultConfig, err := os.ReadFile(filepath.Join(renderChart(t, false), "configmap.yaml"))
	if err != nil {
		t.Fatalf("read default rendered config: %v", err)
	}

	for _, want := range []string{`peer_fetch_timeout: "15m"`, "transfer_max_concurrent_serves: 10\n"} {
		if !bytes.Contains(defaultConfig, []byte(want)) {
			t.Fatalf("default rendered config does not contain %q", want)
		}
	}

	outputDir := renderChart(t, false,
		"--set-string", "gantry.peerFetchTimeout=60s",
		"--set", "gantry.transferMaxConcurrentServes=100",
	)

	rawConfig, err := os.ReadFile(filepath.Join(outputDir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}

	for _, want := range []string{`peer_fetch_timeout: "60s"`, "transfer_max_concurrent_serves: 100\n"} {
		if !bytes.Contains(rawConfig, []byte(want)) {
			t.Fatalf("rendered config does not contain %q", want)
		}
	}
}

func TestRendersConfiguredChairAndSeedCounts(t *testing.T) {
	t.Parallel()

	outputDir := renderChart(t, false,
		"--set", "gantry.chairCount=128",
		"--set", "gantry.chairSeedCount=100",
	)

	rawConfig, err := os.ReadFile(filepath.Join(outputDir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}

	if !bytes.Contains(rawConfig, []byte("chair_count: 128")) {
		t.Fatal("rendered config does not contain chair_count: 128")
	}

	if !bytes.Contains(rawConfig, []byte("chair_seed_count: 100")) {
		t.Fatal("rendered config does not contain chair_seed_count: 100")
	}

	if !bytes.Contains(rawConfig, []byte(`chair_lease_duration: "5m"`)) {
		t.Fatal("rendered config does not contain five-minute chair Lease duration")
	}

	if !bytes.Contains(rawConfig, []byte(`chair_renew_period: "1m"`)) {
		t.Fatal("rendered config does not contain one-minute chair renewal period")
	}

	rawLeases, err := os.ReadFile(filepath.Join(outputDir, "rendezvous-leases.yaml"))
	if err != nil {
		t.Fatalf("read rendered chairs: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(rawLeases))
	count := 0

	for {
		var object struct {
			Kind string `yaml:"kind"`
		}
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode chair manifest: %v", err)
		}

		if object.Kind == "Lease" {
			count++
		}
	}

	if count != 128 {
		t.Fatalf("chair Lease count = %d, want 128", count)
	}
}

func TestDaemonSetUsesChairNamespaceWithoutMembershipInputs(t *testing.T) {
	t.Parallel()

	outputDir := renderTemplates(t)

	raw, err := os.ReadFile(filepath.Join(outputDir, "daemonset.yaml"))
	if err != nil {
		t.Fatalf("read rendered daemonset: %v", err)
	}

	var daemonSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `yaml:"name"`
						Env  []struct {
							Name string `yaml:"name"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &daemonSet); err != nil {
		t.Fatalf("unmarshal rendered daemonset: %v", err)
	}

	environment := map[string]bool{}

	for _, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name != "gantry" {
			continue
		}

		for _, variable := range container.Env {
			environment[variable.Name] = true
		}
	}

	if !environment["GANTRY_CHAIR_NAMESPACE"] {
		t.Fatal("GANTRY_CHAIR_NAMESPACE is missing")
	}

	for _, obsolete := range []string{"GANTRY_NODE_NAME", "GANTRY_MEMBERS_NAMESPACE"} {
		if environment[obsolete] {
			t.Fatalf("obsolete informer input %s is still present", obsolete)
		}
	}
}

func TestChairRBACAllowsLeaseRecovery(t *testing.T) {
	t.Parallel()

	outputDir := renderTemplates(t)

	raw, err := os.ReadFile(filepath.Join(outputDir, "serviceaccount.yaml"))
	if err != nil {
		t.Fatalf("read rendered service account: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	foundLeaseRule := false
	foundDaemonSetRule := false

	for {
		var object struct {
			Kind  string `yaml:"kind"`
			Rules []struct {
				APIGroups []string `yaml:"apiGroups"`
				Resources []string `yaml:"resources"`
				Verbs     []string `yaml:"verbs"`
			} `yaml:"rules"`
		}
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode service account manifests: %v", err)
		}

		if object.Kind != "Role" {
			continue
		}

		for _, rule := range object.Rules {
			if containsString(rule.APIGroups, "coordination.k8s.io") && containsString(rule.Resources, "leases") {
				for _, verb := range []string{"get", "list", "create", "update"} {
					if !containsString(rule.Verbs, verb) {
						t.Fatalf("Lease RBAC verbs = %v, missing %q", rule.Verbs, verb)
					}
				}

				foundLeaseRule = true
			}

			if containsString(rule.APIGroups, "apps") && containsString(rule.Resources, "daemonsets") {
				if !containsString(rule.Verbs, "get") {
					t.Fatalf("DaemonSet RBAC verbs = %v, missing get", rule.Verbs)
				}

				foundDaemonSetRule = true
			}
		}
	}

	if !foundLeaseRule {
		t.Fatal("no coordination Lease RBAC rule rendered")
	}

	if !foundDaemonSetRule {
		t.Fatal("no apps DaemonSet RBAC rule rendered")
	}
}

func TestStandaloneProfileContinuouslyReconcilesNodeConfig(t *testing.T) {
	t.Parallel()

	standaloneDir := renderStandaloneTemplates(t)
	standaloneObjects := renderedObjects(t, standaloneDir)
	operatorObjects := renderedObjects(t, renderTemplates(t))

	for _, key := range []string{
		"ConfigMap/unbounded-system/gantry-containerd-hosts",
		"DaemonSet/unbounded-system/gantry-containerd-config",
	} {
		if _, ok := standaloneObjects[key]; !ok {
			t.Fatalf("standalone profile is missing %s", key)
		}

		if _, ok := operatorObjects[key]; ok {
			t.Fatalf("operator profile unexpectedly contains %s", key)
		}
	}

	raw, err := os.ReadFile(filepath.Join(standaloneDir, "node-config.yaml"))
	if err != nil {
		t.Fatalf("read rendered node config: %v", err)
	}

	manifest := string(raw)
	for _, fragment := range []string{
		"path: /etc/containerd/certs.d",
		"path: _default/hosts.toml",
		"for source_file in /config/*/hosts.toml; do",
		"relative_path=${source_file#/config/}",
		"while true; do",
		"if ! cmp -s \"$source_file\" \"$target_file\"; then",
		"mv \"$temp_file\" \"$target_file\"",
		"sleep 5",
		"rm -f \"$target_file\"",
	} {
		if !strings.Contains(manifest, fragment) {
			t.Fatalf("standalone node config is missing %q", fragment)
		}
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))

	var hostsConfig, reconcileScript string

	for {
		var object struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
			Spec struct {
				Selector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"selector"`
				Template struct {
					Spec struct {
						Containers []struct {
							Name    string   `yaml:"name"`
							Command []string `yaml:"command"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode node config: %v", err)
		}

		switch object.Kind {
		case "ConfigMap":
			hostsConfig = object.Data["hosts.toml"]
		case "DaemonSet":
			if got := object.Spec.Selector.MatchLabels["app.kubernetes.io/name"]; got != "gantry-containerd-config" {
				t.Fatalf("node-config selector name = %q, want gantry-containerd-config", got)
			}

			for _, container := range object.Spec.Template.Spec.Containers {
				if container.Name == "configure" && len(container.Command) == 3 {
					reconcileScript = container.Command[2]
				}
			}
		}
	}

	if hostsConfig == "" || reconcileScript == "" {
		t.Fatal("rendered node config is missing its payload or reconcile script")
	}

	hostRoot := t.TempDir()
	sourceRoot := t.TempDir()
	targetDir := filepath.Join(hostRoot, "_default")

	sourceDir := filepath.Join(sourceRoot, "_default")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}

	sourceFile := filepath.Join(sourceDir, "hosts.toml")
	if err := os.WriteFile(sourceFile, []byte(hostsConfig), 0o644); err != nil {
		t.Fatalf("write source hosts config: %v", err)
	}

	reconcileScript = strings.ReplaceAll(reconcileScript, "/host-certs", hostRoot)
	reconcileScript = strings.ReplaceAll(reconcileScript, "/config", sourceRoot)
	reconcileScript = strings.Replace(reconcileScript, "sleep 5", "sleep 0.05", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", reconcileScript)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node config reconciler: %v", err)
	}

	defer func() {
		cancel()

		_ = cmd.Wait()
	}()

	targetFile := filepath.Join(targetDir, "hosts.toml")
	waitForFileContent(t, targetFile, hostsConfig)

	if err := os.WriteFile(targetFile, []byte("node upgrade reset\n"), 0o644); err != nil {
		t.Fatalf("replace managed hosts config: %v", err)
	}

	waitForFileContent(t, targetFile, hostsConfig)
}

func TestArtifactStreamingUsesAKSMirrorChain(t *testing.T) {
	t.Parallel()

	const registry = "gantrystreamacr1.azurecr.io"

	outputDir := renderChart(t, false,
		"--set", "gantry.artifactStreaming.enabled=true",
		"--set-string", "gantry.upstreamRegistries[0].name="+registry,
		"--set-string", "gantry.upstreamRegistries[0].endpoint=http://127.0.0.1:8578?ns="+registry,
	)

	daemonSetRaw, err := os.ReadFile(filepath.Join(outputDir, "daemonset.yaml"))
	if err != nil {
		t.Fatalf("read rendered daemonset: %v", err)
	}

	var daemonSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					HostNetwork bool   `yaml:"hostNetwork"`
					DNSPolicy   string `yaml:"dnsPolicy"`
					Containers  []struct {
						Name  string `yaml:"name"`
						Ports []struct {
							Name     string `yaml:"name"`
							HostPort int    `yaml:"hostPort"`
						} `yaml:"ports"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(daemonSetRaw, &daemonSet); err != nil {
		t.Fatalf("unmarshal rendered daemonset: %v", err)
	}

	if !daemonSet.Spec.Template.Spec.HostNetwork {
		t.Fatal("artifact-streaming Gantry hostNetwork = false, want true")
	}

	if got := daemonSet.Spec.Template.Spec.DNSPolicy; got != "ClusterFirstWithHostNet" {
		t.Fatalf("artifact-streaming Gantry dnsPolicy = %q", got)
	}

	for _, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name != "gantry" {
			continue
		}

		for _, port := range container.Ports {
			if port.Name == "mirror" && port.HostPort != 0 {
				t.Fatalf("host-networked mirror hostPort = %d, want omitted", port.HostPort)
			}
		}
	}

	configRaw, err := os.ReadFile(filepath.Join(outputDir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered configmap: %v", err)
	}

	for _, fragment := range []string{
		`mirror_listen: "127.0.0.1:5000"`,
		`endpoint: "http://127.0.0.1:8578?ns=` + registry + `"`,
	} {
		if !strings.Contains(string(configRaw), fragment) {
			t.Fatalf("artifact-streaming config missing %q:\n%s", fragment, configRaw)
		}
	}

	nodeConfigRaw, err := os.ReadFile(filepath.Join(outputDir, "node-config.yaml"))
	if err != nil {
		t.Fatalf("read rendered node config: %v", err)
	}

	manifest := string(nodeConfigRaw)
	for _, fragment := range []string{
		`server = "https://` + registry + `"`,
		`[host."http://127.0.0.1:5000"]`,
		`[host."http://127.0.0.1:8578"]`,
		"path: " + registry + "/hosts.toml",
	} {
		if !strings.Contains(manifest, fragment) {
			t.Fatalf("AKS node config missing %q:\n%s", fragment, manifest)
		}
	}

	if strings.Contains(manifest, "path: _default/hosts.toml") {
		t.Fatalf("AKS node config overwrites _default:\n%s", manifest)
	}
}

func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil && string(content) == want {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s did not converge to the desired content", path)
}

func TestStandaloneAndOperatorProfilesShareCoreResources(t *testing.T) {
	t.Parallel()

	operatorObjects := renderedObjects(t, renderTemplates(t))
	standaloneObjects := renderedObjects(t, renderStandaloneTemplates(t))

	delete(operatorObjects, "Namespace//unbounded-system")

	const overlayBDConfigKey = "DaemonSet/unbounded-system/gantry-overlaybd-config"
	if _, ok := operatorObjects[overlayBDConfigKey]; !ok {
		t.Fatalf("operator profile is missing %s", overlayBDConfigKey)
	}

	delete(operatorObjects, overlayBDConfigKey)
	delete(standaloneObjects, "ConfigMap/unbounded-system/gantry-containerd-hosts")
	delete(standaloneObjects, "DaemonSet/unbounded-system/gantry-containerd-config")

	if len(operatorObjects) != len(standaloneObjects) {
		t.Fatalf("shared object counts differ: operator=%d standalone=%d", len(operatorObjects), len(standaloneObjects))
	}

	keys := make([]string, 0, len(operatorObjects))
	for key := range operatorObjects {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		operatorObject := normalizeProfileObject(operatorObjects[key])

		standaloneObject, ok := standaloneObjects[key]
		if !ok {
			t.Fatalf("standalone profile is missing %s", key)
		}

		if !reflect.DeepEqual(operatorObject, normalizeProfileObject(standaloneObject)) {
			t.Fatalf("shared object %s differs between profiles\noperator: %#v\nstandalone: %#v", key, operatorObject, normalizeProfileObject(standaloneObject))
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}

	return false
}

func renderTemplates(t *testing.T) string {
	t.Helper()

	return renderChart(t, true)
}

func renderStandaloneTemplates(t *testing.T) string {
	t.Helper()

	return renderChart(t, false)
}

func renderChart(t *testing.T, operatorProfile bool, extraArgs ...string) string {
	t.Helper()

	deployDir := filepath.Dir(sourceFile(t))
	repositoryDir := filepath.Clean(filepath.Join(deployDir, "..", ".."))

	helm := filepath.Join(repositoryDir, "bin", "helm")
	if _, err := os.Stat(helm); err != nil {
		var lookupErr error

		helm, lookupErr = exec.LookPath("helm")
		if lookupErr != nil {
			t.Fatal("helm not found; run make install-helm")
		}
	}

	outputRoot := t.TempDir()

	args := []string{
		"template", "gantry", filepath.Join(deployDir, "chart"),
		"--namespace", "unbounded-system",
		"--set-string", "image.reference=gantry:test",
		"--output-dir", outputRoot,
	}
	if operatorProfile {
		args = append(
			args,
			"--values", filepath.Join(deployDir, "chart", "values-operator.yaml"),
			"--skip-schema-validation",
		)
	}

	args = append(args, extraArgs...)

	cmd := exec.Command(helm, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render Gantry chart: %v\n%s", err, output)
	}

	return filepath.Join(outputRoot, "gantry", "templates")
}

func renderedObjects(t *testing.T, directory string) map[string]map[string]any {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(directory, "*.yaml"))
	if err != nil {
		t.Fatalf("list rendered manifests: %v", err)
	}

	objects := make(map[string]map[string]any)

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		decoder := yaml.NewDecoder(bytes.NewReader(raw))

		for {
			var object map[string]any
			if err := decoder.Decode(&object); err != nil {
				if err == io.EOF {
					break
				}

				t.Fatalf("decode %s: %v", file, err)
			}

			if len(object) == 0 {
				continue
			}

			metadata, ok := object["metadata"].(map[string]any)
			if !ok {
				t.Fatalf("%s object has no metadata", file)
			}

			kind, _ := object["kind"].(string)
			namespace, _ := metadata["namespace"].(string)
			name, _ := metadata["name"].(string)

			key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
			if _, exists := objects[key]; exists {
				t.Fatalf("duplicate rendered object %s", key)
			}

			objects[key] = object
		}
	}

	return objects
}

func normalizeProfileObject(object map[string]any) map[string]any {
	normalized := deepCopyMap(object)
	deleteMapKey(normalized, "metadata", "labels", "app.kubernetes.io/managed-by")
	deleteMapKey(normalized, "metadata", "annotations", "gantry.unbounded-cloud.io/manager")
	deleteMapKey(normalized, "metadata", "annotations", "meta.helm.sh/release-name")
	deleteMapKey(normalized, "metadata", "annotations", "meta.helm.sh/release-namespace")
	deleteMapKey(normalized, "spec", "template", "metadata", "labels", "app.kubernetes.io/managed-by")
	deleteMapKey(normalized, "spec", "template", "metadata", "annotations", "checksum/config")
	deleteEmptyMap(normalized, "metadata", "annotations")
	deleteEmptyMap(normalized, "spec", "template", "metadata", "annotations")

	return normalized
}

func deepCopyMap(value map[string]any) map[string]any {
	copy := make(map[string]any, len(value))
	for key, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			copy[key] = deepCopyMap(typed)
		case []any:
			items := make([]any, len(typed))
			for index, element := range typed {
				if child, ok := element.(map[string]any); ok {
					items[index] = deepCopyMap(child)
				} else {
					items[index] = element
				}
			}

			copy[key] = items
		default:
			copy[key] = item
		}
	}

	return copy
}

func deleteMapKey(object map[string]any, path ...string) {
	current := object
	for _, part := range path[:len(path)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return
		}

		current = next
	}

	delete(current, path[len(path)-1])
}

func deleteEmptyMap(object map[string]any, path ...string) {
	current := object
	for _, part := range path[:len(path)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return
		}

		current = next
	}

	if value, ok := current[path[len(path)-1]].(map[string]any); ok && len(value) == 0 {
		delete(current, path[len(path)-1])
	}
}

func sourceFile(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	return file
}

func TestRendersConfiguredAgentUpdateStrategy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "maxUnavailable: 50%"},
		{
			name: "override",
			args: []string{"--set-string", "updateStrategy.rollingUpdate.maxUnavailable=10%"},
			want: "maxUnavailable: 10%",
		},
		{
			name: "count",
			args: []string{"--set", "updateStrategy.rollingUpdate.maxUnavailable=3"},
			want: "maxUnavailable: 3\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(renderChart(t, false, tc.args...), "daemonset.yaml"))
			if err != nil {
				t.Fatalf("read rendered agent DaemonSet: %v", err)
			}

			if !bytes.Contains(raw, []byte(tc.want)) {
				t.Fatalf("rendered agent DaemonSet does not contain %q", tc.want)
			}
		})
	}
}
