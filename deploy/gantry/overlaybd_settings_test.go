// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOverlayBDContainersShareConfiguredEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:5000/blobs", "http://127.0.0.1:6000/blobs", "http://localhost:6123/blobs"} {
		t.Run(endpoint, func(t *testing.T) {
			directory := renderChart(t, false,
				"--set", "gantry.artifactStreaming.enabled=true",
				"--set", "overlaybdConfig.enabled=true",
				"--set-string", "overlaybdConfig.nodeSelector.pool=streaming",
				"--set-string", "overlaybdConfig.gantryEndpoint="+endpoint,
				"--set-string", "overlaybdConfig.address=/run/gantry-overlaybd/custom.sock",
				"--set-string", "overlaybdConfig.snapshotterRoot=/mnt/image-disk/gantry",
			)

			raw, err := os.ReadFile(filepath.Join(directory, "overlaybd-config.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			var daemonSet struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Name string                         `yaml:"name"`
								Args []string                       `yaml:"args"`
								Env  []struct{ Name, Value string } `yaml:"env"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(raw, &daemonSet); err != nil {
				t.Fatal(err)
			}

			var snapshotter, readiness, socket, storageRoot bool

			for _, container := range daemonSet.Spec.Template.Spec.Containers {
				if container.Name == "snapshotter" {
					snapshotter = slices.Contains(container.Args, "--gantry-endpoint="+endpoint) &&
						slices.Contains(container.Args, "--address=/run/gantry-overlaybd/custom.sock") &&
						slices.Contains(container.Args, "--root=/mnt/image-disk/gantry")
				}

				if container.Name != "configure" {
					continue
				}

				for _, env := range container.Env {
					if env.Name == "GANTRY_STREAMING_READYZ" {
						readiness = env.Value == strings.TrimSuffix(endpoint, "/blobs")+"/artifact-streaming/readyz"
					}

					if env.Name == "GANTRY_SNAPSHOTTER_ADDRESS" {
						socket = env.Value == "/run/gantry-overlaybd/custom.sock"
					}

					if env.Name == "GANTRY_SNAPSHOTTER_ROOT" {
						storageRoot = env.Value == "/mnt/image-disk/gantry"
					}
				}
			}

			if !snapshotter || !readiness || !socket || !storageRoot {
				t.Fatalf("snapshotter/readiness/socket/root agree = %v/%v/%v/%v\n%s", snapshotter, readiness, socket, storageRoot, raw)
			}
		})
	}
}

func TestOverlayBDRejectsSocketOutsideSharedDirectory(t *testing.T) {
	deployDir := filepath.Dir(sourceFile(t))
	helm := filepath.Clean(filepath.Join(deployDir, "..", "..", "bin", "helm"))

	for _, address := range []string{"/run/custom/snapshotter.sock", "/run/gantry-overlaybd/../snapshotter.sock", "/run/gantry-overlaybd/nested/snapshotter.sock", "/run/gantry-overlaybd"} {
		for _, skipSchema := range []bool{false, true} {
			args := []string{"template", "gantry", filepath.Join(deployDir, "chart"), "--set-string", "overlaybdConfig.address=" + address}
			if skipSchema {
				args = append(args, "--skip-schema-validation")
			}

			output, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "overlaybdConfig") {
				t.Fatalf("address %q (skip schema %v): error = %v\n%s", address, skipSchema, err, output)
			}
		}
	}
}
