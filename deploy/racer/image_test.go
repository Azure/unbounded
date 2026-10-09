// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestNativeRDMAImageContract(t *testing.T) {
	read := func(name string) string {
		t.Helper()

		data, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	image := read("images/racer-dataplane/Containerfile")
	if strings.Count(image, "ARG RACER_NATIVE_RDMA=true") != 2 || strings.Contains(image, "ARG RACER_NATIVE_RDMA=false") {
		t.Fatal("both builder and runtime must default to native RDMA")
	}

	for _, required := range []string{
		"libibverbs-dev pkg-config", "libibverbs1 ibverbs-providers",
		"--no-default-features --features rdma", "librdma_verbs.so.1", "RUN ldconfig",
		"COPY cmd/racer-dataplane/controlplane/Cargo.toml ./controlplane/Cargo.toml",
		"COPY cmd/racer-dataplane/controlplane/src ./controlplane/src",
		"COPY cmd/racer-dataplane/http/tests ./http/tests",
	} {
		if !strings.Contains(image, required) {
			t.Fatalf("native image contract missing %q", required)
		}
	}

	for _, name := range []string{"release", "nightly", "ci"} {
		t.Run(name, func(t *testing.T) {
			text := read(".github/workflows/" + name + ".yaml")

			var workflow struct {
				Jobs map[string]struct {
					Strategy struct {
						Matrix struct {
							Component []struct {
								Name      string
								BuildArgs string `json:"build-args"`
							}
						}
					}
					Steps []struct {
						Run  string
						With map[string]any
					}
				}
			}
			if err := yaml.Unmarshal([]byte(text), &workflow); err != nil {
				t.Fatal(err)
			}

			found := false

			for _, job := range workflow.Jobs {
				for _, component := range job.Strategy.Matrix.Component {
					if name == "release" && component.Name == "racer-dataplane" {
						if component.BuildArgs != "RACER_NATIVE_RDMA=true" {
							t.Fatal("release matrix must explicitly enable native RDMA")
						}

						builds := 0

						for _, step := range job.Steps {
							if step.With["file"] == "${{ matrix.component.file }}" {
								args, _ := step.With["build-args"].(string)
								if !strings.Contains(args, "${{ matrix.component.build-args }}") {
									t.Fatal("scan and push must both consume native build arguments")
								}

								builds++
							}
						}

						found = builds == 2
					}
				}

				for _, step := range job.Steps {
					if name == "ci" && step.With["file"] == "images/racer-dataplane/Containerfile" {
						args, _ := step.With["build-args"].(string)
						found = strings.Contains(args, "RACER_NATIVE_RDMA=true")
					}

					if name == "nightly" && strings.Contains(step.Run, "racer-dataplane)") {
						found = strings.Contains(step.Run, `echo "RACER_NATIVE_RDMA=true"`)
					}
				}
			}

			if !found {
				t.Fatal("workflow must explicitly build native Racer images")
			}
		})
	}
}

func TestControllerImageBuildMetadata(t *testing.T) {
	data, err := os.ReadFile("../../images/racer-controller/Containerfile")
	require.NoError(t, err)

	image := string(data)
	for field, argument := range map[string]string{"Version": "VERSION", "GitCommit": "GIT_COMMIT", "BuildTime": "BUILD_TIME"} {
		require.Contains(t, image, "ARG "+argument+"=")
		require.Contains(t, image, "-X github.com/Azure/unbounded/internal/version."+field+"=${"+argument+"}")
	}

	require.Contains(t, image, "-o /out/racer-controller ./cmd/racer-controller")
	require.Contains(t, image, "COPY --from=builder /out/racer-controller /usr/local/bin/racer-controller")
}

// Every local crate needs its manifest and sources in the isolated image build.
func TestDataplaneImageCopiesLocalCrates(t *testing.T) {
	data, err := os.ReadFile("../../images/racer-dataplane/Containerfile")
	require.NoError(t, err)

	manifests, err := filepath.Glob("../../cmd/racer-dataplane/*/Cargo.toml")
	require.NoError(t, err)
	require.NotEmpty(t, manifests)

	for _, manifest := range manifests {
		member := filepath.Base(filepath.Dir(manifest))
		for _, input := range []string{"Cargo.toml", "src"} {
			require.Contains(t, string(data), "COPY cmd/racer-dataplane/"+member+"/"+input+" ./"+member+"/"+input+"\n")
		}
	}
}

// Moving a library test into a helper integration suite must not hide it from this gate.
func TestDataplaneTestDiscoversHelperIntegrationSuites(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	_, target, found := strings.Cut(string(data), "\nracer-dataplane-test:")
	require.True(t, found)

	target, _, _ = strings.Cut(target, "\n\n")
	require.Contains(t, target, "--workspace --exclude racer-dataplane --all-features --tests")
}
