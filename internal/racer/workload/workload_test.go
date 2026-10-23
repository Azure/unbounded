// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func nvidiaLookup(mode string, set bool) func(string) (string, bool) {
	values := map[string]string{
		"RACER_CLUSTER_ID":      "00000000-0000-4000-8000-000000000001",
		"RACER_CONTROL_URL":     "https://racer-controller.unbounded-system.svc:8443",
		"RACER_DATAPLANE_IMAGE": "racer-dataplane:test",
	}
	if set {
		values["RACER_DATAPLANE_NVIDIA"] = mode
	}

	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}

	return "", false
}

func TestNVIDIAInjectionModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		set        bool
		wantEnv    bool
		wantCDI    bool
		wantConfig NVIDIAInjection
	}{
		{name: "default", wantEnv: true},
		{name: "off", mode: "off", set: true, wantConfig: NVIDIAOff},
		{name: "env", mode: "env", set: true, wantEnv: true, wantConfig: NVIDIAEnv},
		{name: "cdi", mode: "cdi", set: true, wantEnv: true, wantCDI: true, wantConfig: NVIDIACDI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ConfigFromLookup(nvidiaLookup(tc.mode, tc.set))
			if err != nil {
				t.Fatal(err)
			}

			if cfg.NVIDIA != tc.wantConfig {
				t.Fatalf("NVIDIA = %q, want %q", cfg.NVIDIA, tc.wantConfig)
			}

			daemonSets, err := DesiredDaemonSets(cfg)
			if err != nil {
				t.Fatal(err)
			}

			template := daemonSets[0].Spec.Template
			container := template.Spec.Containers[0]

			devices, hasDevices := envValue(container, "NVIDIA_VISIBLE_DEVICES")
			caps, hasCaps := envValue(container, "NVIDIA_DRIVER_CAPABILITIES")

			if hasDevices != tc.wantEnv || hasCaps != tc.wantEnv {
				t.Fatalf("NVIDIA env present = %v/%v, want %v", hasDevices, hasCaps, tc.wantEnv)
			}

			if tc.wantEnv && (devices != "all" || caps != "compute,utility") {
				t.Fatalf("NVIDIA env = %q/%q", devices, caps)
			}

			value, hasCDI := template.Annotations[CDIAnnotation]
			if hasCDI != tc.wantCDI || (hasCDI && value != "nvidia.com/gpu=all") {
				t.Fatalf("CDI annotation = %q (present %v), want present %v", value, hasCDI, tc.wantCDI)
			}

			// cuFile classifies slab devices through the host udev database.
			hasUdevVolume := false

			for _, volume := range template.Spec.Volumes {
				if volume.Name == "udev" {
					hasUdevVolume = volume.HostPath != nil && volume.HostPath.Path == "/run/udev"
				}
			}

			hasUdevMount := false

			for _, mount := range container.VolumeMounts {
				if mount.Name == "udev" {
					hasUdevMount = mount.MountPath == "/run/udev" && mount.ReadOnly
				}
			}

			if hasUdevVolume != tc.wantEnv || hasUdevMount != tc.wantEnv {
				t.Fatalf("udev volume/mount = %v/%v, want %v", hasUdevVolume, hasUdevMount, tc.wantEnv)
			}

			// GPU libraries must never come from a GPU resource request.
			for _, list := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
				if _, ok := list["nvidia.com/gpu"]; ok {
					t.Fatal("dataplane must not request nvidia.com/gpu")
				}
			}
		})
	}
}

func TestNVIDIAInjectionRejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "true", "auto", "CDI"} {
		if _, err := ConfigFromLookup(nvidiaLookup(mode, true)); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("mode %q: err = %v, want InvalidRequest", mode, err)
		}
	}

	cfg, err := ConfigFromLookup(nvidiaLookup("", false))
	if err != nil {
		t.Fatal(err)
	}

	cfg.NVIDIA = "bogus"
	if err := cfg.Validate(); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("Validate = %v, want InvalidRequest", err)
	}

	// The zero value keeps the env default for programmatic callers.
	cfg.NVIDIA = ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
