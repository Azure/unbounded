// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestRenderedRuntimeSecurity(t *testing.T) {
	for _, suffix := range []string{"", ".custom", `"quoted\\name`} {
		t.Run(suffix, func(t *testing.T) {
			data := map[string]string{}

			credentials, installation, version := "racer-credentials", "racer-installation", "racer-version"
			if suffix != "" {
				credentials, installation, version = credentials+suffix, installation+suffix, version+suffix
				data = map[string]string{"CredentialsSecretName": credentials, "InstallationConfigMapName": installation, "VersionConfigMapName": version}
			}

			data["InitializationState"] = "fresh"
			data["ClusterID"] = "11111111-1111-1111-1111-111111111111"

			out := t.TempDir()
			require.NoError(t, render.Render(".", out, data))

			decode := func(file string, objects ...any) {
				t.Helper()

				b, err := os.ReadFile(filepath.Join(out, file))
				require.NoError(t, err)

				d := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
				for _, obj := range objects {
					require.NoError(t, d.Decode(obj))
				}
			}

			var config, marker corev1.ConfigMap
			decode("config.yaml", &config)
			decode("installation.yaml", &marker)
			require.Equal(t, credentials, config.Data["RACER_CREDENTIALS_SECRET_NAME"])
			require.Equal(t, installation, config.Data["RACER_INSTALLATION_CONFIGMAP_NAME"])
			require.Equal(t, version, config.Data["RACER_VERSION_CONFIGMAP_NAME"])
			require.Equal(t, installation, marker.Name)
			require.Equal(t, version, marker.Data["version_configmap"])

			var (
				sa, dataplane  corev1.ServiceAccount
				clusterRole    rbacv1.ClusterRole
				clusterBinding rbacv1.ClusterRoleBinding
				role           rbacv1.Role
			)

			decode("rbac.yaml", &sa, &dataplane, &clusterRole, &clusterBinding, &role)

			for _, rule := range append(role.Rules, clusterRole.Rules...) {
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						if resource == "secrets" && verb != "create" {
							require.Equal(t, []string{credentials}, rule.ResourceNames)
						}

						if resource == "configmaps" && (verb == "update" || verb == "patch") {
							require.Equal(t, []string{installation, version}, rule.ResourceNames)
						}
					}
				}
			}

			var policy admissionv1.ValidatingAdmissionPolicy
			decode("create-restriction.yaml", &policy)

			for _, name := range []string{credentials, installation, version} {
				require.Contains(t, policy.Spec.Validations[0].Expression, strconv.Quote(name))
			}

			var deployment appsv1.Deployment
			decode("controller.yaml", &deployment)
			pod := deployment.Spec.Template.Spec
			require.NotNil(t, pod.SecurityContext)
			require.Equal(t, int64(65532), *pod.SecurityContext.RunAsUser)
			require.True(t, *pod.SecurityContext.RunAsNonRoot)
			require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
			container := pod.Containers[0]
			require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
			require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
			require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)

			for _, resource := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				request, limit := container.Resources.Requests[resource], container.Resources.Limits[resource]
				require.True(t, request.Sign() > 0)
				require.True(t, limit.Cmp(request) >= 0)
			}
		})
	}
}
