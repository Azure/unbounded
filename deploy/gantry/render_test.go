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
		"artifact_streaming_peer_lookup_timeout: \"250ms\"",
		"artifact_streaming_max_peer_attempts: 3",
		"artifact_streaming_max_concurrent_origin_reads: 32",
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
			Template struct {
				Spec struct {
					HostPID      bool              `yaml:"hostPID"`
					NodeSelector map[string]string `yaml:"nodeSelector"`
					Containers   []struct {
						Image           string `yaml:"image"`
						SecurityContext struct {
							Privileged bool `yaml:"privileged"`
						} `yaml:"securityContext"`
					} `yaml:"containers"`
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

	if got := daemonSet.Spec.Template.Spec.NodeSelector["kubernetes.azure.com/host-os"]; got != "AzureLinux" {
		t.Errorf("node selector = %q, want AzureLinux", got)
	}

	if len(daemonSet.Spec.Template.Spec.Containers) != 1 ||
		daemonSet.Spec.Template.Spec.Containers[0].Image != "gantry-node-config:test" ||
		!daemonSet.Spec.Template.Spec.Containers[0].SecurityContext.Privileged {
		t.Fatalf("unexpected configurator container: %+v", daemonSet.Spec.Template.Spec.Containers)
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
				Name string `yaml:"name"`
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
		"target_file=\"$target_dir/hosts.toml\"",
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
	targetDir := filepath.Join(hostRoot, "_default")

	sourceFile := filepath.Join(t.TempDir(), "hosts.toml")
	if err := os.WriteFile(sourceFile, []byte(hostsConfig), 0o644); err != nil {
		t.Fatalf("write source hosts config: %v", err)
	}

	reconcileScript = strings.Replace(reconcileScript, "target_dir=/host-certs/_default", fmt.Sprintf("target_dir=%q", targetDir), 1)
	reconcileScript = strings.Replace(reconcileScript, "source_file=/config/hosts.toml", fmt.Sprintf("source_file=%q", sourceFile), 1)
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
