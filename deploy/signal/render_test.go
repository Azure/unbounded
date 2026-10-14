// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestSignalManifestsRender(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, namespace, controllerImage, runnerImage string
		data                                          map[string]string
	}{
		{
			name:            "defaults",
			namespace:       "project-signal",
			controllerImage: "example.invalid/project-signal-controller:replace-me",
			runnerImage:     "example.invalid/gpu-runner:replace-me",
		},
		{
			name:            "overrides",
			namespace:       "signal-test",
			controllerImage: "example.invalid/controller@sha256:reviewed",
			runnerImage:     "example.invalid/runner@sha256:reviewed",
			data: map[string]string{
				"Namespace":       "signal-test",
				"ControllerImage": "example.invalid/controller@sha256:reviewed",
				"RunnerImage":     "example.invalid/runner@sha256:reviewed",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := renderSignalManifests(t, tc.data)
			files, err := filepath.Glob(filepath.Join(output, "*.yaml"))
			require.NoError(t, err)
			require.Len(t, files, 7)

			for _, file := range files {
				content, err := os.ReadFile(file)
				require.NoError(t, err)
				assert.NotContains(t, string(content), "project-signal-dashboard")

				for _, document := range strings.Split(string(content), "\n---") {
					var object struct {
						Kind     string            `json:"kind"`
						Metadata metav1.ObjectMeta `json:"metadata"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(document), &object), file)
					require.NotEmpty(t, object.Kind, file)

					switch object.Kind {
					case "Namespace", "ClusterRole", "ClusterRoleBinding", "CertificationProfile":
						assert.Empty(t, object.Metadata.Namespace, file)
					default:
						assert.Equal(t, tc.namespace, object.Metadata.Namespace, file)
					}
				}
			}

			var namespace corev1.Namespace
			readSignalManifest(t, output, "00-namespace.yaml", &namespace)
			assert.Equal(t, tc.namespace, namespace.Name)

			var deployment appsv1.Deployment
			readSignalManifest(t, output, "06-deployment.yaml", &deployment)
			assert.Equal(t, "project-signal-controller", deployment.Spec.Template.Spec.ServiceAccountName)
			require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
			controller := deployment.Spec.Template.Spec.Containers[0]
			assert.Equal(t, tc.controllerImage, controller.Image)
			assert.Contains(t, controller.Args, "--namespace="+tc.namespace)
			assert.Contains(t, controller.Args, "--script-namespace="+tc.namespace)
			assert.Contains(t, controller.Args, "--leader-elect=true")

			var profile struct {
				Spec struct {
					RunnerImage string `json:"runnerImage"`
				} `json:"spec"`
			}
			readSignalManifest(t, output, "05-profiles.yaml", &profile)
			assert.Equal(t, tc.runnerImage, profile.Spec.RunnerImage)

			content, err := os.ReadFile(filepath.Join(output, "01-serviceaccounts.yaml"))
			require.NoError(t, err)

			accounts := strings.Split(string(content), "\n---")
			require.Len(t, accounts, 2)

			for index, name := range []string{"project-signal-controller", "project-signal-runner"} {
				var account corev1.ServiceAccount
				require.NoError(t, yaml.Unmarshal([]byte(accounts[index]), &account))
				assert.Equal(t, name, account.Name)
				assert.Equal(t, name, account.Labels["app.kubernetes.io/name"])
				require.NotNil(t, account.AutomountServiceAccountToken)
				assert.True(t, *account.AutomountServiceAccountToken)
			}
		})
	}
}

func TestSignalControllerRBACSupportsLifecycleWithoutClusterRunDeletion(t *testing.T) {
	t.Parallel()

	output := renderSignalManifests(t, map[string]string{"Namespace": "signal-test"})
	content, err := os.ReadFile(filepath.Join(output, "02-rbac.yaml"))
	require.NoError(t, err)

	documents := strings.Split(string(content), "\n---")
	require.Len(t, documents, 8)

	type rbacDocument struct {
		Kind     string              `json:"kind"`
		Metadata metav1.ObjectMeta   `json:"metadata"`
		Rules    []rbacv1.PolicyRule `json:"rules"`
		RoleRef  rbacv1.RoleRef      `json:"roleRef"`
		Subjects []rbacv1.Subject    `json:"subjects"`
	}

	roles := make(map[string]rbacDocument)

	for _, document := range documents {
		var object rbacDocument
		require.NoError(t, yaml.Unmarshal([]byte(document), &object))
		roles[object.Kind+"/"+object.Metadata.Name] = object

		if strings.HasSuffix(object.Kind, "Binding") {
			require.Len(t, object.Subjects, 1)
			assert.Equal(t, "ServiceAccount", object.Subjects[0].Kind)
			assert.Equal(t, "signal-test", object.Subjects[0].Namespace)
			assert.Equal(t, "rbac.authorization.k8s.io", object.RoleRef.APIGroup)
		}

		for _, rule := range object.Rules {
			assert.NotContains(t, rule.APIGroups, "*")
			assert.NotContains(t, rule.Resources, "*")
			assert.NotContains(t, rule.Resources, "secrets")
			assert.NotContains(t, rule.Verbs, "*")
		}
	}

	cluster := roles["ClusterRole/project-signal-controller"]
	require.NotEmpty(t, cluster.Kind)

	for _, verb := range []string{"get", "list", "watch", "update", "patch"} {
		assert.True(t, signalRoleGrants(cluster.Rules, "", "nodes", verb), verb)
	}

	for _, resource := range []string{"certificationprofiles", "certificationruns"} {
		for _, verb := range []string{"get", "list", "watch"} {
			assert.True(t, signalRoleGrants(cluster.Rules, "signal.unbounded-cloud.io", resource, verb), "%s/%s", resource, verb)
		}
	}

	assert.False(t, signalRoleGrants(cluster.Rules, "signal.unbounded-cloud.io", "certificationruns", "delete"))
	assert.False(t, signalRoleGrants(cluster.Rules, "signal.unbounded-cloud.io", "certificationruns", "deletecollection"))

	controller := roles["Role/project-signal-controller"]
	assert.Equal(t, "signal-test", controller.Metadata.Namespace)

	for _, tc := range []struct {
		group, resource string
		verbs           []string
	}{
		{"signal.unbounded-cloud.io", "certificationruns", []string{"create", "update", "patch", "delete"}},
		{"", "configmaps", []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{"argoproj.io", "workflows", []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{"kubeflow.org", "mpijobs", []string{"get", "list", "delete"}},
		{"", "events", []string{"create", "patch"}},
		{"events.k8s.io", "events", []string{"create", "patch"}},
		{"coordination.k8s.io", "leases", []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	} {
		for _, verb := range tc.verbs {
			assert.True(t, signalRoleGrants(controller.Rules, tc.group, tc.resource, verb), "%s/%s/%s", tc.group, tc.resource, verb)
		}
	}

	runner := roles["Role/project-signal-runner"]
	assert.Equal(t, "signal-test", runner.Metadata.Namespace)

	for _, verb := range []string{"create", "patch"} {
		assert.True(t, signalRoleGrants(runner.Rules, "argoproj.io", "workflowtaskresults", verb), verb)
	}

	for _, verb := range []string{"get", "list", "watch", "create", "patch", "delete"} {
		assert.True(t, signalRoleGrants(runner.Rules, "kubeflow.org", "mpijobs", verb), verb)
	}

	for _, name := range []string{"project-signal-controller", "project-signal-runner", "project-signal-script-reader"} {
		role := roles["Role/"+name]
		binding := roles["RoleBinding/"+name]
		require.NotEmpty(t, role.Kind, name)
		require.NotEmpty(t, binding.Kind, name)
		assert.Equal(t, role.Metadata.Namespace, binding.Metadata.Namespace)
		assert.Equal(t, "Role", binding.RoleRef.Kind)
		assert.Equal(t, name, binding.RoleRef.Name)

		serviceAccount := name
		if name == "project-signal-script-reader" {
			serviceAccount = "project-signal-controller"
		}

		assert.Equal(t, serviceAccount, binding.Subjects[0].Name)
	}

	binding := roles["ClusterRoleBinding/project-signal-controller"]
	assert.Equal(t, "ClusterRole", binding.RoleRef.Kind)
	assert.Equal(t, cluster.Metadata.Name, binding.RoleRef.Name)
	assert.Equal(t, "project-signal-controller", binding.Subjects[0].Name)
}

func TestExampleProfilesRemainSingleNodeAndFailClosed(t *testing.T) {
	t.Parallel()

	output := renderSignalManifests(t, nil)

	for _, path := range []string{filepath.Join(output, "05-profiles.yaml"), "samples/gb200-single-node-smoke.yaml"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			content, err := os.ReadFile(path)
			require.NoError(t, err)

			var profile struct {
				Spec struct {
					Participants struct {
						Count             int                  `json:"count"`
						Selector          metav1.LabelSelector `json:"selector"`
						RequiredResources corev1.ResourceList  `json:"requiredResources"`
					} `json:"participants"`
					Script struct {
						Name string `json:"name"`
						Key  string `json:"key"`
					} `json:"script"`
					Parameters map[string]string `json:"parameters"`
					Purpose    string            `json:"purpose"`
					StagePlan  []struct {
						ID          string `json:"id"`
						DisplayName string `json:"displayName"`
						Required    bool   `json:"required"`
					} `json:"stagePlan"`
					Deadline    string `json:"deadline"`
					MaxAttempts int    `json:"maxAttempts"`
					Recurring   struct {
						Enabled   bool     `json:"enabled"`
						NodeNames []string `json:"nodeNames"`
						ExpiresAt string   `json:"expiresAt"`
					} `json:"recurring"`
				} `json:"spec"`
			}
			require.NoError(t, yaml.Unmarshal(content, &profile))
			assert.Equal(t, 1, profile.Spec.Participants.Count)
			gpu := profile.Spec.Participants.RequiredResources["nvidia.com/gpu"]
			assert.Equal(t, int64(1), gpu.Value())
			assert.Contains(t, profile.Spec.Participants.Selector.MatchLabels["node.kubernetes.io/instance-type"], "REPLACE_WITH_VERIFIED")
			assert.Contains(t, profile.Spec.Participants.Selector.MatchLabels["nvidia.com/gpu.product"], "REPLACE_WITH_VERIFIED")
			assert.Equal(t, "project-signal-scripts", profile.Spec.Script.Name)
			assert.Equal(t, "gpu-identity-example.sh", profile.Spec.Script.Key)
			assert.Contains(t, profile.Spec.Parameters["expected-gpu-product-regex"], "REPLACE_WITH_VERIFIED")
			assert.Equal(t, "REPLACE_WITH_VERIFIED_GPU_COUNT", profile.Spec.Parameters["expected-gpu-count"])
			assert.Equal(t, "15m", profile.Spec.Deadline)
			assert.Equal(t, 2, profile.Spec.MaxAttempts)
			assert.False(t, profile.Spec.Recurring.Enabled)
			assert.Equal(t, "1970-01-01T00:00:00Z", profile.Spec.Recurring.ExpiresAt)
			require.Len(t, profile.Spec.Recurring.NodeNames, 1)
			assert.Contains(t, profile.Spec.Recurring.NodeNames[0], "REPLACE_WITH_APPROVED")

			if path == "samples/gb200-single-node-smoke.yaml" {
				assert.NotContains(t, string(content), "stagePlan:")
				assert.NotContains(t, string(content), "purpose:")
			} else {
				assert.Equal(t, "QuickCertification", profile.Spec.Purpose)
				require.Len(t, profile.Spec.StagePlan, 2)

				for index, id := range []string{"node-health", "mpi-backend"} {
					assert.Equal(t, id, profile.Spec.StagePlan[index].ID)
					assert.NotEmpty(t, profile.Spec.StagePlan[index].DisplayName)
					assert.True(t, profile.Spec.StagePlan[index].Required)
				}
			}
		})
	}
}

func renderSignalManifests(t *testing.T, data map[string]string) string {
	t.Helper()

	output := t.TempDir()
	require.NoError(t, render.Render(".", output, data))

	return output
}

func readSignalManifest(t *testing.T, output, name string, destination any) {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(output, name))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(content, destination))
}

func signalRoleGrants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) &&
			slices.Contains(rule.Resources, resource) &&
			slices.Contains(rule.Verbs, verb) {
			return true
		}
	}

	return false
}
