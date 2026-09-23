// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

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

func TestStandaloneAndOperatorProfilesShareCoreResources(t *testing.T) {
	t.Parallel()

	operatorObjects := renderedObjects(t, renderTemplates(t))
	standaloneObjects := renderedObjects(t, renderStandaloneTemplates(t))

	delete(operatorObjects, "Namespace//unbounded-system")

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

func renderChart(t *testing.T, operatorProfile bool) string {
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
