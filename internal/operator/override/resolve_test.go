// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCheckVolumeCollisionsRejectsRedefiningAnOperatorVolume is a regression
// test for a bypass of the mount protection.
//
// Mounts are protected on (container, mountPath), the key strategic merge uses
// for volumeMounts, so a patch cannot repoint an operator mount at a different
// volume. Volumes themselves merge on name, so a patch could leave every mount
// alone and redefine what the volume is instead. On workloads that already run
// privileged and host-networked, silently repointing a hostPath is the most
// consequential edit an override can make, and it named no mountPath at all.
func TestCheckVolumeCollisionsRejectsRedefiningAnOperatorVolume(t *testing.T) {
	workload := workloadWithVolumes(map[string]string{"host-run": "/run", "config": ""})

	problems := checkResolvable(Entry{Patch: patchWithVolumes(`
              - name: host-run
                hostPath:
                  path: /etc/kubernetes
`)}, Source{Key: "a.yaml"}, workload)

	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one collision", problems)
	}

	if !strings.Contains(problems[0].Error(), "merge on name") {
		t.Fatalf("problem = %q, want it to explain why the name matters", problems[0])
	}
}

// TestCheckVolumeCollisionsAllowsNewVolumes keeps the field usable: adding a
// volume is the point, and only names the operator already declares are
// refused.
func TestCheckVolumeCollisionsAllowsNewVolumes(t *testing.T) {
	workload := workloadWithVolumes(map[string]string{"host-run": "/run"})

	problems := checkResolvable(Entry{Patch: patchWithVolumes(`
              - name: sidecar-scratch
                emptyDir: {}
`)}, Source{Key: "a.yaml"}, workload)

	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none for a new volume", problems)
	}
}

// TestCheckVolumeCollisionsIgnoresNameOnlyEntries covers the entry that changes
// nothing. It is inert under strategic merge, so failing a whole document over
// it would be noise.
func TestCheckVolumeCollisionsIgnoresNameOnlyEntries(t *testing.T) {
	workload := workloadWithVolumes(map[string]string{"host-run": "/run"})

	problems := checkResolvable(Entry{Patch: patchWithVolumes("\n              - name: host-run\n")},
		Source{Key: "a.yaml"}, workload)

	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none for an entry that changes nothing", problems)
	}
}

// workloadWithVolumes builds a DaemonSet declaring the named volumes, using a
// hostPath when a path is given and an emptyDir otherwise.
func workloadWithVolumes(volumes map[string]string) *unstructured.Unstructured {
	names := make([]string, 0, len(volumes))
	for name := range volumes {
		names = append(names, name)
	}

	sort.Strings(names)

	declared := make([]any, 0, len(names))

	for _, name := range names {
		volume := map[string]any{"name": name, "emptyDir": map[string]any{}}
		if path := volumes[name]; path != "" {
			volume = map[string]any{"name": name, "hostPath": map[string]any{"path": path}}
		}

		declared = append(declared, volume)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "agent"},
	}}

	if err := unstructured.SetNestedSlice(obj.Object, declared, "spec", "template", "spec", "volumes"); err != nil {
		panic(err)
	}

	return obj
}

// patchWithVolumes parses a volumes fragment into a patch map.
func patchWithVolumes(fragment string) map[string]any {
	var patch struct {
		Patch map[string]any `yaml:"patch"`
	}

	doc := "patch:\n  spec:\n    template:\n      spec:\n        volumes:" + fragment

	if err := yaml.Unmarshal([]byte(doc), &patch); err != nil {
		panic(err)
	}

	normalized, err := normalizeJSON(patch.Patch, "patch")
	if err != nil {
		panic(err)
	}

	return normalized.(map[string]any)
}
