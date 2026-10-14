// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestRuntimeRenderingExcludesCertificationOwnedInputs(t *testing.T) {
	t.Parallel()

	for _, namespace := range []string{"", "signal-custom"} {
		t.Run("namespace="+namespace, func(t *testing.T) {
			t.Parallel()

			files := renderSignalFiles(t, ".", map[string]string{
				"Namespace":       namespace,
				"RunnerImage":     "example.invalid/must-not-select-a-runtime-runner:rejected",
				"ControllerImage": "example.invalid/reviewed-controller@sha256:controller",
				"DashboardImage":  "example.invalid/reviewed-dashboard@sha256:dashboard",
			})
			require.Len(t, files, 9)

			for _, name := range []string{
				"00-namespace.yaml",
				"01-serviceaccounts.yaml",
				"02-rbac.yaml",
				"04-workflowtemplate.yaml",
				"06-deployment.yaml",
				"07-dashboard.yaml",
				"08-full-node-workflowtemplate.yaml",
				"09-burn-workflowtemplate.yaml",
				"10-rack-workflowtemplate.yaml",
			} {
				require.Contains(t, files, name)
			}

			expectedNamespace := namespace
			if expectedNamespace == "" {
				expectedNamespace = "project-signal"
			}

			var workflows, deployments []string

			for name, content := range files {
				for _, forbidden := range []string{
					"must-not-select-a-runtime-runner",
					"projectsignalpublic.azurecr.io",
					"kechomicrosoft/project-signal",
					"Dapple_B200",
					"gpu-07-",
					"project-signal-example-scripts",
				} {
					require.NotContains(t, string(content), forbidden, name)
				}

				decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(content)), 4096)

				for {
					var object struct {
						Kind     string `json:"kind"`
						Metadata struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						} `json:"metadata"`
						Subjects []struct {
							Namespace string `json:"namespace"`
						} `json:"subjects"`
					}

					err := decoder.Decode(&object)
					if err == io.EOF {
						break
					}

					require.NoError(t, err, name)
					require.Contains(t, []string{
						"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding",
						"Role", "RoleBinding", "Deployment", "Service", "WorkflowTemplate",
					}, object.Kind, name)

					switch object.Kind {
					case "Namespace":
						require.Equal(t, expectedNamespace, object.Metadata.Name)
						require.Empty(t, object.Metadata.Namespace)
					case "ClusterRole", "ClusterRoleBinding":
						require.Empty(t, object.Metadata.Namespace)
					default:
						require.Equal(t, expectedNamespace, object.Metadata.Namespace, name)
					}

					for _, subject := range object.Subjects {
						require.Equal(t, expectedNamespace, subject.Namespace, name)
					}

					switch object.Kind {
					case "WorkflowTemplate":
						workflows = append(workflows, object.Metadata.Name)
					case "Deployment":
						deployments = append(deployments, object.Metadata.Name)
					}
				}
			}

			require.ElementsMatch(t, []string{
				"project-signal-gpu-certification",
				"project-signal-gpu-full-node-certification",
				"project-signal-gpu-burn-canary",
				"project-signal-rack-certification",
			}, workflows)
			require.ElementsMatch(t, []string{"project-signal-controller", "project-signal-dashboard"}, deployments)
			require.Contains(t, string(files["06-deployment.yaml"]), "--namespace="+expectedNamespace)
			require.Contains(t, string(files["06-deployment.yaml"]), "--script-namespace="+expectedNamespace)
			require.Contains(t, string(files["06-deployment.yaml"]), "example.invalid/reviewed-controller@sha256:controller")
			require.Contains(t, string(files["07-dashboard.yaml"]), "--namespace="+expectedNamespace)
			require.Contains(t, string(files["07-dashboard.yaml"]), "example.invalid/reviewed-dashboard@sha256:dashboard")
		})
	}
}

func TestSmokeRenderingIsOptInAndRetainsIdentityOnlyCompatibility(t *testing.T) {
	t.Parallel()

	for _, namespace := range []string{"", "signal-custom"} {
		t.Run("namespace="+namespace, func(t *testing.T) {
			t.Parallel()

			values := map[string]string{"Namespace": namespace}
			expectedNamespace := "project-signal"
			expectedImage := "example.invalid/gpu-runner:replace-me"

			if namespace != "" {
				expectedNamespace = namespace
				expectedImage = "example.invalid/reviewed-runner@sha256:runner"
				values["RunnerImage"] = expectedImage
			}

			files := renderSignalFiles(t, "../signal-examples", values)
			require.Len(t, files, 2)
			require.Contains(t, files, "03-scripts.yaml")
			require.Contains(t, files, "05-profiles.yaml")

			var scripts corev1.ConfigMap
			require.NoError(t, yaml.Unmarshal(files["03-scripts.yaml"], &scripts))
			require.Equal(t, "ConfigMap", scripts.Kind)
			require.Equal(t, "project-signal-example-scripts", scripts.Name)
			require.Equal(t, expectedNamespace, scripts.Namespace)
			require.Equal(t, map[string]string{
				"gpu-identity-example.sh": certificationScript(t, "gpu-identity-example.sh"),
			}, scripts.Data)

			var profile signalv1.CertificationProfile
			require.NoError(t, yaml.Unmarshal(files["05-profiles.yaml"], &profile))
			require.Equal(t, "CertificationProfile", profile.Kind)
			require.Equal(t, "gpu-smoke-example", profile.Name)
			require.Empty(t, profile.Namespace)
			require.Equal(t, scripts.Name, profile.Spec.Script.Name)
			require.Equal(t, "gpu-identity-example.sh", profile.Spec.Script.Key)
			require.Equal(t, "legacy-inline", profile.Spec.Parameters["mpi-payload-mode"])
			require.Equal(t, "project-signal-gpu-certification", profile.Spec.Workflow.Name)
			require.Equal(t, "kubeflow.org/v2beta1", profile.Spec.Backend.APIVersion)
			require.Equal(t, "MPIJob", profile.Spec.Backend.Kind)
			require.Equal(t, expectedImage, profile.Spec.RunnerImage)
			require.Equal(t, int32(1), profile.Spec.Participants.Count)
			require.Empty(t, profile.Spec.Participants.NodeNames)
			require.Empty(t, profile.Spec.Participants.TopologyKey)
			require.Equal(t, map[string]string{
				"node.kubernetes.io/instance-type": "REPLACE_WITH_VERIFIED_INSTANCE_TYPE",
				"nvidia.com/gpu.product":           "REPLACE_WITH_VERIFIED_GPU_PRODUCT_LABEL",
			}, profile.Spec.Participants.Selector.MatchLabels)
			gpus := profile.Spec.Participants.RequiredResources["nvidia.com/gpu"]
			require.Equal(t, int64(1), gpus.Value())
			require.Equal(t, "^REPLACE_WITH_VERIFIED_NVIDIA_SMI_PRODUCT$", profile.Spec.Parameters["expected-gpu-product-regex"])
			require.Equal(t, "REPLACE_WITH_VERIFIED_GPU_COUNT", profile.Spec.Parameters["expected-gpu-count"])
			require.Equal(t, 15*time.Minute, profile.Spec.Deadline.Duration)
			require.Equal(t, int32(2), profile.Spec.MaxAttempts)
			require.NotNil(t, profile.Spec.Recurring)
			require.False(t, profile.Spec.Recurring.Enabled)
			require.Equal(t, []string{"REPLACE_WITH_APPROVED_NODE"}, profile.Spec.Recurring.NodeNames)
			require.Equal(t, "1970-01-01T00:00:00Z", profile.Spec.Recurring.ExpiresAt.UTC().Format(time.RFC3339))
			require.Equal(t, 15*time.Minute, profile.Spec.Recurring.MinIdleDuration.Duration)
			require.Equal(t, time.Hour, profile.Spec.Recurring.ValidFor.Duration)
			require.Equal(t, time.Minute, profile.Spec.Recurring.EvaluationInterval.Duration)
			require.Equal(t, 30*time.Minute, profile.Spec.Recurring.FailureBackoff.Duration)
			require.Equal(t, int32(2), profile.Spec.Recurring.MaxConsecutiveFailures)
			require.Equal(t, int32(1), profile.Spec.Recurring.MaxConcurrent)
			require.Equal(t, "topology.kubernetes.io/zone", profile.Spec.Recurring.FailureDomainLabel)
			require.Equal(t, int32(1), profile.Spec.Recurring.MaxConcurrentPerFailureDomain)
			require.Empty(t, profile.Spec.Recurring.ExclusiveProfiles)
		})
	}
}

func TestGB200SmokeSampleConsumesOptInIdentity(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("samples/gb200-single-node-smoke.yaml")
	require.NoError(t, err)

	var profile signalv1.CertificationProfile
	require.NoError(t, yaml.Unmarshal(raw, &profile))
	require.Equal(t, "project-signal-example-scripts", profile.Spec.Script.Name)
	require.Equal(t, "gpu-identity-example.sh", profile.Spec.Script.Key)
	require.Equal(t, "legacy-inline", profile.Spec.Parameters["mpi-payload-mode"])
	require.Equal(t, "project-signal-gpu-certification", profile.Spec.Workflow.Name)
	require.Equal(t, int32(1), profile.Spec.Participants.Count)
	require.Contains(t, profile.Spec.RunnerImage, "REPLACE_WITH_")
	require.Contains(t, profile.Spec.Parameters["expected-gpu-product-regex"], "REPLACE_WITH_VERIFIED_")
	require.Equal(t, "REPLACE_WITH_VERIFIED_GPU_COUNT", profile.Spec.Parameters["expected-gpu-count"])
	require.Equal(t, 15*time.Minute, profile.Spec.Deadline.Duration)
	require.Equal(t, int32(2), profile.Spec.MaxAttempts)
	require.NotNil(t, profile.Spec.Recurring)
	require.False(t, profile.Spec.Recurring.Enabled)
	require.Equal(t, []string{"REPLACE_WITH_APPROVED_GB200_NODE"}, profile.Spec.Recurring.NodeNames)
	require.Equal(t, "1970-01-01T00:00:00Z", profile.Spec.Recurring.ExpiresAt.UTC().Format(time.RFC3339))
}

func TestLegacyScriptDecodedDigestsRemainUnchanged(t *testing.T) {
	t.Parallel()

	for name, digest := range map[string]string{
		"gpu-identity-example.sh": "sha256:82f1275c8864938b19f548a5f0411d171627fee4dc4440dba49f1ea26b73606a",
		"gpu-full-node.sh":        "sha256:663677b5c13fa8a0c21756e64aed458186601d876b069482528f38961a6183de",
		"gpu-burn-canary.sh":      "sha256:9329bb7ca8a5dd7157a33fac94d8833b6c1e27868a6dcf1a7078336a61a118df",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			script := certificationScript(t, name)
			require.Equal(t, digest, fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(script))))
		})
	}
}

func renderSignalFiles(t *testing.T, source string, values map[string]string) map[string][]byte {
	t.Helper()

	output := t.TempDir()
	require.NoError(t, render.Render(source, output, values))

	files := map[string][]byte{}
	err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		name, err := filepath.Rel(output, path)
		if err != nil {
			return err
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		files[name] = content

		return nil
	})
	require.NoError(t, err)

	return files
}
