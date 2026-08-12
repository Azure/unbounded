// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"errors"
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

// TestCheckExclusiveFieldsRejectsWhatCannotBeExpressed covers the class of
// mistake created by refusing explicit null.
//
// An override cannot delete anything, because strategic merge treats null as
// deletion and that is how operator-managed content would be removed. A patch
// can therefore only add to what is already there, and where the Kubernetes
// schema says two fields may not both be present, adding one is not enough:
// the result is an object the API server refuses, discovered at apply time as
// an error about a field the user never wrote.
func TestCheckExclusiveFieldsRejectsWhatCannotBeExpressed(t *testing.T) {
	t.Run("value over an operator valueFrom", func(t *testing.T) {
		workload := workloadWithEnv("agent", "SECRET", map[string]any{
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}},
		})

		problems := checkResolvable(Entry{Patch: patchWithEnv("agent", "SECRET", "value: literal")},
			Source{Key: "a.yaml"}, workload)

		if len(problems) != 1 {
			t.Fatalf("problems = %v, want the exclusivity rejected", problems)
		}

		if !strings.Contains(problems[0].Error(), "only one") {
			t.Fatalf("problem = %q, want it to explain the constraint", problems[0])
		}
	})

	t.Run("valueFrom over an operator value", func(t *testing.T) {
		workload := workloadWithEnv("agent", "LEVEL", map[string]any{"value": "info"})

		problems := checkResolvable(
			Entry{Patch: patchWithEnv("agent", "LEVEL", "valueFrom:\n                      configMapKeyRef:\n                        name: c\n                        key: k")},
			Source{Key: "a.yaml"}, workload)

		if len(problems) != 1 {
			t.Fatalf("problems = %v, want the exclusivity rejected", problems)
		}
	})

	t.Run("changing the value of a plain env is fine", func(t *testing.T) {
		workload := workloadWithEnv("agent", "LEVEL", map[string]any{"value": "info"})

		problems := checkResolvable(Entry{Patch: patchWithEnv("agent", "LEVEL", "value: debug")},
			Source{Key: "a.yaml"}, workload)

		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: this is the ordinary case", problems)
		}
	})

	t.Run("a new env variable is fine", func(t *testing.T) {
		workload := workloadWithEnv("agent", "LEVEL", map[string]any{"value": "info"})

		problems := checkResolvable(Entry{Patch: patchWithEnv("agent", "NEW", "value: x")},
			Source{Key: "a.yaml"}, workload)

		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: the operator defines no NEW", problems)
		}
	})
}

// TestCheckStrategyExclusivity covers the second reachable case: Recreate
// cannot coexist with a rollingUpdate block, and the override cannot remove the
// operator's.
func TestCheckStrategyExclusivity(t *testing.T) {
	withRollingUpdate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "controller"},
		"spec": map[string]any{
			"strategy": map[string]any{
				"type":          "RollingUpdate",
				"rollingUpdate": map[string]any{"maxSurge": int64(0)},
			},
		},
	}}

	problems := checkResolvable(
		Entry{Patch: map[string]any{"spec": map[string]any{"strategy": map[string]any{"type": "Recreate"}}}},
		Source{Key: "a.yaml"}, withRollingUpdate)

	if len(problems) != 1 {
		t.Fatalf("problems = %v, want Recreate rejected against an operator rollingUpdate", problems)
	}

	// Without an operator rollingUpdate there is nothing to conflict with.
	plain := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "controller"},
		"spec":     map[string]any{"strategy": map[string]any{"type": "RollingUpdate"}},
	}}

	if problems := checkResolvable(
		Entry{Patch: map[string]any{"spec": map[string]any{"strategy": map[string]any{"type": "Recreate"}}}},
		Source{Key: "a.yaml"}, plain); len(problems) != 0 {
		t.Fatalf("problems = %v, want none when the operator sets no rollingUpdate", problems)
	}
}

// workloadWithEnv builds a workload whose container defines one env variable.
func workloadWithEnv(container, variable string, definition map[string]any) *unstructured.Unstructured {
	env := map[string]any{"name": variable}
	for key, value := range definition {
		env[key] = value
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "agent"},
	}}

	if err := unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"name": container, "env": []any{env}},
	}, "spec", "template", "spec", "containers"); err != nil {
		panic(err)
	}

	return obj
}

// patchWithEnv builds a patch setting one env entry on one container.
func patchWithEnv(container, variable, definition string) map[string]any {
	return patchFromYAML(`
patch:
  spec:
    template:
      spec:
        containers:
          - name: ` + container + `
            env:
              - name: ` + variable + `
                ` + definition + `
`)
}

// patchFromYAML parses a `patch:` document into a normalized patch map.
func patchFromYAML(doc string) map[string]any {
	var parsed struct {
		Patch map[string]any `yaml:"patch"`
	}

	if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
		panic(err)
	}

	normalized, err := normalizeJSON(parsed.Patch, "patch")
	if err != nil {
		panic(err)
	}

	out, _ := normalized.(map[string]any)

	return out
}

// TestCheckResolvableRejectsSelectorLabelRewrites is a regression test.
//
// spec.selector is protected, but spec.template.metadata.labels is a permitted
// subtree, so a patch changing a selector-matched label validated, merged, and
// was then silently restamped by restoreIdentity. The user got no error and no
// effect, which is the silent-no-op class this feature exists to avoid.
func TestCheckResolvableRejectsSelectorLabelRewrites(t *testing.T) {
	workload := testWorkload("rack-a")

	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"app.kubernetes.io/name": "mine"},
				},
			},
		},
	}

	problems := checkResolvable(Entry{Patch: patch}, Source{Key: "a.yaml"}, workload)
	if len(problems) == 0 {
		t.Fatal("rewriting a selector-matched template label must be rejected, not silently restored")
	}

	if !strings.Contains(errors.Join(problems...).Error(), "selector matches") {
		t.Fatalf("problems = %v, want the selector named", problems)
	}
}

// TestCheckResolvableAllowsUnrelatedTemplateLabels pins that adding a label the
// selector does not match is still the supported thing to do.
func TestCheckResolvableAllowsUnrelatedTemplateLabels(t *testing.T) {
	workload := testWorkload("rack-a")

	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"team": "platform",
						// Restating a selector label with the value it already
						// has changes nothing and must not be refused.
						"app.kubernetes.io/name": "storage",
					},
				},
			},
		},
	}

	if problems := checkResolvable(Entry{Patch: patch}, Source{Key: "a.yaml"}, workload); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}
