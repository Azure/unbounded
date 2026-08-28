// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The rendered RBAC must grant exactly the fixed Lease slots and nothing
// else: the Pod and Node grants the membership path required are gone, and
// the ClusterRole is retained-but-empty so applying these manifests revokes
// a grant a previous release held.
func TestRenderGrantsOnlyFixedLeaseSlots(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	if err := render.Render(filepath.Dir(sourceFile(t)), outputDir, map[string]string{
		"Namespace":           "unbounded-system",
		"Image":               "gantry:test",
		"RendezvousSlotCount": "4",
	}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		return string(raw)
	}

	serviceAccount := read("serviceaccount.yaml")
	daemonSet := read("daemonset.yaml")
	leases := read("rendezvous-leases.yaml")

	for _, forbidden := range []string{`resources: ["pods"]`, `resources: ["nodes"]`} {
		if strings.Contains(serviceAccount, forbidden) {
			t.Errorf("serviceaccount still grants %s", forbidden)
		}
	}

	if !strings.Contains(serviceAccount, `resources: ["leases"]`) {
		t.Error("serviceaccount does not grant the Lease slots")
	}

	if !strings.Contains(serviceAccount, "kind: ClusterRole") || !strings.Contains(serviceAccount, "rules:\n  []") {
		t.Error("serviceaccount must retain an empty ClusterRole so the old Node grant is revoked")
	}

	if got := strings.Count(leases, "kind: Lease"); got != 4 {
		t.Errorf("Lease count = %d, want 4", got)
	}

	for i := range 4 {
		name := fmt.Sprintf("gantry-rendezvous-%04d", i)
		if !strings.Contains(serviceAccount, name) {
			t.Errorf("serviceaccount does not name slot %s", name)
		}
	}

	if strings.Contains(daemonSet, "GANTRY_MEMBERS_NAMESPACE") {
		t.Error("daemonset still wires the membership informer namespace")
	}

	// single_node grants readiness before any peer is dialed, so a retained
	// ConfigMap must never be able to supply it.
	if !strings.Contains(daemonSet, "name: GANTRY_RENDEZVOUS_SINGLE_NODE\n              value: \"false\"") {
		t.Error("daemonset does not force single_node off")
	}

	for _, required := range []string{"GANTRY_RENDEZVOUS_SLOT_COUNT", "GANTRY_RENDEZVOUS_NAMESPACE", "GANTRY_NF5_JITTER_CAP"} {
		if !strings.Contains(daemonSet, "name: "+required) {
			t.Errorf("daemonset does not inject %s", required)
		}
	}
}

func TestRenderConfigTriesAllBoundedDHTProviders(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	if err := render.Render(filepath.Dir(sourceFile(t)), outputDir, map[string]string{
		"Namespace": "unbounded-system",
		"Image":     "gantry:test",
	}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(outputDir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered configmap: %v", err)
	}

	if !strings.Contains(string(raw), "peer_max_attempts: 20") {
		t.Fatalf("rendered config does not try all 20 providers returned by a bounded DHT lookup")
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
