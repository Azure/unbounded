// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestDaemonSetRuntimeMountAndStartupProbe(t *testing.T) {
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

	type probe struct {
		HTTPGet struct {
			Path string `yaml:"path"`
			Port string `yaml:"port"`
		} `yaml:"httpGet"`
		PeriodSeconds    int32 `yaml:"periodSeconds"`
		TimeoutSeconds   int32 `yaml:"timeoutSeconds"`
		FailureThreshold int32 `yaml:"failureThreshold"`
		SuccessThreshold int32 `yaml:"successThreshold"`
	}

	var daemonSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string `yaml:"name"`
						StartupProbe *probe `yaml:"startupProbe"`
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

	var (
		mountPath, subPath, mountPropagation string
		startupProbe                         *probe
	)

	for _, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name != "gantry" {
			continue
		}

		startupProbe = container.StartupProbe

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

	if startupProbe == nil {
		t.Fatal("gantry startup probe not found")
	}

	if startupProbe.HTTPGet.Path != "/livez" || startupProbe.HTTPGet.Port != "metrics" {
		t.Fatalf("startup probe HTTP endpoint = %s:%s, want metrics:/livez", startupProbe.HTTPGet.Port, startupProbe.HTTPGet.Path)
	}

	if startupProbe.PeriodSeconds != 10 || startupProbe.TimeoutSeconds != 1 ||
		startupProbe.FailureThreshold != 190 || startupProbe.SuccessThreshold != 1 {
		t.Fatalf("startup probe timing = period %ds, timeout %ds, failure threshold %d, success threshold %d; want 10s, 1s, 190, 1",
			startupProbe.PeriodSeconds, startupProbe.TimeoutSeconds,
			startupProbe.FailureThreshold, startupProbe.SuccessThreshold)
	}

	startupBudget := time.Duration(startupProbe.PeriodSeconds) *
		time.Duration(startupProbe.FailureThreshold) * time.Second
	if startupBudget != 31*time.Minute+40*time.Second {
		t.Fatalf("startup probe budget = %s, want 31m40s", startupBudget)
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

func sourceFile(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	return file
}
