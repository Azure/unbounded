// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

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
