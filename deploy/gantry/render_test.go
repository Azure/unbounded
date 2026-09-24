// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestRacerStandalonePatch(t *testing.T) {
	for _, name := range []string{"gantry", "custom-cache", "0", strings.Repeat("a", 63)} {
		t.Run(name, func(t *testing.T) {
			testRacerStandalonePatch(t, name, false)
			t.Run("legacy root mount", func(t *testing.T) {
				testRacerStandalonePatch(t, name, true)
			})
		})
	}
}

func testRacerStandalonePatch(t *testing.T, name string, legacyRoot bool) {
	t.Helper()

	output := t.TempDir()
	if err := render.Render(filepath.Dir(sourceFile(t)), output, map[string]string{"RacerCacheName": name}); err != nil {
		t.Fatal(err)
	}

	base, err := os.ReadFile(filepath.Join(output, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	patch, err := os.ReadFile(filepath.Join(output, "examples/racer-daemonset-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	baseJSON, err := kubeyaml.YAMLToJSON(base)
	if err != nil {
		t.Fatal(err)
	}

	if legacyRoot {
		var ds appsv1.DaemonSet
		if err := json.Unmarshal(baseJSON, &ds); err != nil {
			t.Fatal(err)
		}

		pod := &ds.Spec.Template.Spec
		mount := corev1.VolumeMount{Name: "racer-sockets", MountPath: "/run/racer"}
		pod.InitContainers[0].VolumeMounts = append(pod.InitContainers[0].VolumeMounts, mount)
		pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts, mount)
		pod.InitContainers[0].Command = []string{"sh", "-ec", "chmod 2770 /run/racer"}
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "racer-sockets", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/racer"}}})

		baseJSON, err = json.Marshal(ds)
		if err != nil {
			t.Fatal(err)
		}
	}

	patchJSON, err := kubeyaml.YAMLToJSON(patch)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := strategicpatch.StrategicMergePatch(baseJSON, patchJSON, appsv1.DaemonSet{})
	if err != nil {
		t.Fatal(err)
	}

	var ds appsv1.DaemonSet
	if err := json.Unmarshal(merged, &ds); err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.SecurityContext == nil || !slices.Equal(pod.SecurityContext.SupplementalGroups, []int64{65532}) {
		t.Fatal("Racer origin group/token configuration missing")
	}

	directories := "/run/racer/" + name + "/client /run/racer/" + name + "/origin"

	wantCommand := []string{"sh", "-ec", "chown -R 65532:65532 /var/lib/gantry/libp2p\nchmod 0700 /var/lib/gantry/libp2p\nchgrp 65532 " + directories + "\nchmod 2770 " + directories + "\n"}
	if len(pod.InitContainers) != 1 || !slices.Equal(pod.InitContainers[0].Command, wantCommand) {
		t.Fatalf("init must change permissions only on mounted directories: %#v", pod.InitContainers)
	}

	if ds.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-name"] != name || !slices.Contains(pod.Containers[0].Args, "--racer-cache-name="+name) || !slices.Contains(pod.Containers[0].Args, "--content-backend=racer") {
		t.Fatal("standalone patch must select the supplied cache name")
	}

	for _, c := range pod.Containers {
		for _, port := range c.Ports {
			if port.ContainerPort == 5001 || port.ContainerPort == 5002 {
				t.Fatal("direct port retained")
			}
		}
	}

	wantMounts := map[string]corev1.VolumeMount{
		"racer-client-sockets": {Name: "racer-client-sockets", MountPath: "/run/racer/" + name + "/client"},
		"racer-origin-sockets": {Name: "racer-origin-sockets", MountPath: "/run/racer/" + name + "/origin"},
	}

	wantPaths := map[string]string{}
	for volumeName, mount := range wantMounts {
		wantPaths[volumeName] = mount.MountPath
	}

	paths := map[string]string{}

	for _, volume := range pod.Volumes {
		if !strings.HasPrefix(volume.Name, "racer-") && (volume.HostPath == nil || !strings.HasPrefix(volume.HostPath.Path, "/run/racer")) {
			continue
		}

		if volume.HostPath == nil || volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectoryOrCreate {
			t.Fatalf("Racer volume must be DirectoryOrCreate: %#v", volume)
		}

		paths[volume.Name] = volume.HostPath.Path
	}

	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("Racer hostPaths = %v, want exactly %v", paths, wantPaths)
	}

	for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		mounts := map[string]corev1.VolumeMount{}

		for _, mount := range c.VolumeMounts {
			if strings.HasPrefix(mount.Name, "racer-") || strings.HasPrefix(mount.MountPath, "/run/racer") {
				if _, duplicate := mounts[mount.Name]; duplicate {
					t.Fatalf("%s has duplicate Racer mount %s", c.Name, mount.Name)
				}

				mounts[mount.Name] = mount
			}
		}

		if !reflect.DeepEqual(mounts, wantMounts) {
			t.Fatalf("%s Racer mounts = %#v, want exactly %#v (no socket files or subPaths)", c.Name, mounts, wantMounts)
		}
	}

	cacheRaw, err := os.ReadFile(filepath.Join(output, "examples/racer-cache.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var cache struct {
		Kind     string
		Metadata struct {
			Name        string
			Annotations map[string]string
		}
	}
	if err := yaml.Unmarshal(cacheRaw, &cache); err != nil {
		t.Fatal(err)
	}

	if cache.Kind != "ClusterCache" || cache.Metadata.Name != name || len(cache.Metadata.Annotations) != 0 {
		t.Fatalf("example must match the explicitly selected cache without selection annotations: %#v", cache)
	}
}

func TestRacerStandaloneNameParameter(t *testing.T) {
	for _, name := range []string{"", "a", strings.Repeat("a", 63), "../unsafe", "a;id", "UPPER", "-edge", "edge-", strings.Repeat("a", 64)} {
		t.Run("name="+name, func(t *testing.T) {
			output := t.TempDir()
			err := render.Render(filepath.Dir(sourceFile(t)), output, map[string]string{"RacerCacheName": name})

			valid := name == "" || name == "a" || name == strings.Repeat("a", 63)
			if (err == nil) != valid {
				t.Fatalf("name %q: %v", name, err)
			}

			if name == "" {
				raw, err := os.ReadFile(filepath.Join(output, "examples/racer-daemonset-patch.yaml"))
				if err != nil {
					t.Fatal(err)
				}

				var patch any
				if err := yaml.Unmarshal(raw, &patch); err != nil || patch != nil {
					t.Fatalf("default direct render must not invent a cache name: %s, %v", raw, err)
				}
			}
		})
	}
}

func TestDaemonSetMountsContainerdRuntimeDirectory(t *testing.T) {
	t.Parallel()

	templatesDir := filepath.Dir(sourceFile(t))
	outputDir := t.TempDir()

	if err := render.Render(templatesDir, outputDir, map[string]string{
		"Namespace": "unbounded-system",
		"Image":     "gantry:test",
	}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

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
	found := false

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

				found = true
			}
		}
	}

	if !found {
		t.Fatal("no coordination Lease RBAC rule rendered")
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

	outputDir := t.TempDir()
	if err := render.Render(filepath.Dir(sourceFile(t)), outputDir, map[string]string{
		"Namespace": "unbounded-system",
		"Image":     "gantry:test",
	}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

	return outputDir
}

func sourceFile(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	return file
}
