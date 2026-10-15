// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

func TestRacerChartProfile(t *testing.T) {
	t.Parallel()

	for _, operator := range []bool{false, true} {
		name := "standalone"
		if operator {
			name = "operator"
		}

		t.Run(name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				var args []string
				if enabled {
					args = []string{"--values", filepath.Join(filepath.Dir(sourceFile(t)), "chart", "values-racer.yaml")}
				}

				directory := renderChart(t, operator, args...)
				ds := readChartDaemonSet(t, directory)
				pod := ds.Spec.Template.Spec
				agent := pod.Containers[0]
				security := agent.SecurityContext

				wantUID := int64(65532)
				if enabled {
					wantUID = 0
				}

				if security == nil || security.RunAsUser == nil || *security.RunAsUser != wantUID ||
					security.RunAsNonRoot == nil || *security.RunAsNonRoot == enabled ||
					security.RunAsGroup == nil || *security.RunAsGroup != 0 {
					t.Fatalf("racer=%t: incompatible socket identity: %+v", enabled, security)
				}

				if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
					security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
					(security.Privileged != nil && *security.Privileged) ||
					security.Capabilities == nil || len(security.Capabilities.Add) != 0 ||
					!reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
					security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Fatalf("racer=%t: agent hardening changed: %+v", enabled, security)
				}

				foundEnv := false

				for _, variable := range agent.Env {
					if variable.Name == "GANTRY_RACER_ENABLED" {
						if foundEnv || variable.Value != "true" || variable.ValueFrom != nil {
							t.Fatalf("invalid Racer environment: %+v", agent.Env)
						}

						foundEnv = true
					}
				}

				if foundEnv != enabled {
					t.Fatalf("racer=%t: activation environment present=%t", enabled, foundEnv)
				}

				foundMount, foundVolume := false, false

				for _, mount := range agent.VolumeMounts {
					if mount.Name == "racer-sockets" {
						foundMount = true

						if mount.MountPath != "/run/racer/gantry" || mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" || mount.MountPropagation != nil {
							t.Fatalf("socket directory must permit origin creation and replacement: %+v", mount)
						}
					}
				}

				for _, volume := range pod.Volumes {
					if volume.Name == "racer-sockets" {
						foundVolume = true

						if volume.HostPath == nil || volume.HostPath.Path != "/run/racer/gantry" || volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectoryOrCreate {
							t.Fatalf("invalid canonical socket hostPath: %+v", volume)
						}
					}

					if enabled && (volume.Name == "containerd-runtime" || volume.Name == "libp2p") {
						t.Fatalf("Racer profile retains unused host access: %+v", volume)
					}
				}

				if foundMount != enabled || foundVolume != enabled {
					t.Fatalf("racer=%t: socket mount=%t volume=%t", enabled, foundMount, foundVolume)
				}

				if enabled && len(pod.InitContainers) != 0 {
					t.Fatalf("Racer profile must not chown shared directories: %+v", pod.InitContainers)
				}
			}
		})
	}
}

func TestRacerChartSchema(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"racer.enabled=invalid", "racer.enabled=1", "racer.socketMode=0777", "racer.runAsUser=65532", "racer.enabld=true"} {
		t.Run(value, func(t *testing.T) {
			if _, output, err := runChart(t, false, "--set", value); err == nil {
				t.Fatalf("schema accepted %q: %s", value, output)
			}
		})
	}

	// The internal operator profile skips the public ownership schema, but must
	// still reject a truthy string instead of silently enabling privileged UID 0.
	if _, output, err := runChart(t, true, "--set-string", "racer.enabled=false"); err == nil {
		t.Fatalf("operator profile accepted a string activation value: %s", output)
	}
}

func TestRacerConfigPassthrough(t *testing.T) {
	t.Parallel()

	config := "mirror_listen: 0.0.0.0:5000\nracer_max_connections: 96\n"

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	directory := renderChart(t, false, "--set", "racer.enabled=true", "--set-file", "gantry.config="+path)

	raw, err := os.ReadFile(filepath.Join(directory, "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}

	if cm.Data["config.yaml"] != config {
		t.Fatalf("Racer config was rewritten: %q", cm.Data["config.yaml"])
	}
}

func TestRacerOperatorOverrideMatchesChartSocketAccess(t *testing.T) {
	t.Parallel()

	deployDir := filepath.Dir(sourceFile(t))

	raw, err := os.ReadFile(filepath.Join(deployDir, "examples", "racer-operator-overrides.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}

	entries, problems, err := override.Parse(cm.Data)
	if err != nil || len(problems) != 0 {
		t.Fatalf("parse Racer override: %v %v", err, problems)
	}

	if err := override.ValidateErr(entries); err != nil {
		t.Fatal(err)
	}

	legacy := readChartDaemonSet(t, renderChart(t, true))
	plan := component.NewPlan()
	plan.Add(component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(&legacy), Component: "gantry", Overridable: true})

	if report := override.Apply(plan, entries, nil); report.Failed() || len(report.Workloads) != 1 {
		t.Fatalf("apply Racer override: %+v", report)
	}

	var actual appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(plan.Operations[0].Object.Object, &actual); err != nil {
		t.Fatal(err)
	}

	want := readChartDaemonSet(t, renderChart(t, true, "--values", filepath.Join(deployDir, "chart", "values-racer.yaml")))
	// Overrides intentionally cannot remove operator-managed content. The
	// inherited init only mounts libp2p, never Racer's directory. Normalize the
	// retained legacy resources before comparing the effective Racer pod spec.
	for _, init := range actual.Spec.Template.Spec.InitContainers {
		for _, mount := range init.VolumeMounts {
			if mount.Name != "libp2p" {
				t.Fatalf("operator init has unexpected host access: %+v", mount)
			}
		}
	}

	actual.Spec.Template.Spec.InitContainers = nil
	actual.Spec.Template.Spec.Volumes = slices.DeleteFunc(actual.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == "libp2p" || v.Name == "containerd-runtime"
	})
	agent := &actual.Spec.Template.Spec.Containers[0]
	agent.VolumeMounts = slices.DeleteFunc(agent.VolumeMounts, func(v corev1.VolumeMount) bool {
		return v.Name == "libp2p" || v.Name == "containerd-runtime"
	})
	agent.Ports = slices.DeleteFunc(agent.Ports, func(p corev1.ContainerPort) bool {
		return p.Name == "transfer" || p.Name == "chaircall" || p.Name == "libp2p-tcp"
	})

	if !reflect.DeepEqual(actual.Spec.Template.Spec, want.Spec.Template.Spec) {
		t.Fatalf("operator override diverges from Racer chart pod\nactual: %+v\nwant: %+v", actual.Spec.Template.Spec, want.Spec.Template.Spec)
	}
}

func readChartDaemonSet(t *testing.T, directory string) appsv1.DaemonSet {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(directory, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var ds appsv1.DaemonSet
	if err := yaml.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}

	return ds
}
