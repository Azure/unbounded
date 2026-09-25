// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/racer"
)

func TestRenderedDeploymentWorkloadContract(t *testing.T) {
	for _, namespace := range []string{"unbounded-system", "custom-racer"} {
		t.Run(namespace, func(t *testing.T) {
			out := t.TempDir()

			data := map[string]string{
				"Namespace": namespace, "ClusterID": "11111111-1111-1111-1111-111111111111",
				"ControllerImage": "registry/controller:test", "DataplaneImage": "registry/dataplane:test",
				"BootstrapTrustConfigMap": "deployment-trust", "ServingTLSSecret": "deployment-tls",
				"BootstrapCA": "test-public-ca\nsecond-line\n",
			}
			if err := render.Render(".", out, data); err != nil {
				t.Fatal(err)
			}

			decode := func(file string, objects ...any) {
				t.Helper()

				b, err := os.ReadFile(filepath.Join(out, file))
				if err != nil {
					t.Fatal(err)
				}

				d := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
				for _, object := range objects {
					if err := d.Decode(object); err != nil {
						t.Fatal(err)
					}
				}

				var extra any
				if err := d.Decode(&extra); err != io.EOF {
					t.Fatalf("unexpected extra object: %v", err)
				}
			}

			var config, trust corev1.ConfigMap
			decode("config.yaml", &config)
			decode("bootstrap-trust.yaml", &trust)

			for key, value := range config.Data {
				t.Setenv(key, value)
			}

			t.Setenv("POD_NAMESPACE", namespace)

			cfg, err := racer.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}

			ds, err := (&racer.WorkloadReconciler{Config: cfg}).DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			if ds.Namespace != namespace || ds.Spec.Template.Spec.Containers[0].Image != data["DataplaneImage"] {
				t.Fatal("workload config not wired")
			}

			if trust.Namespace != namespace || trust.Name != cfg.BootstrapTrustConfigMap || trust.Data["ca.crt"] != data["BootstrapCA"] {
				t.Fatal("deployment trust not wired")
			}

			var (
				deployment appsv1.Deployment
				service    corev1.Service
			)

			decode("controller.yaml", &deployment, &service)

			pod := deployment.Spec.Template.Spec
			if deployment.Namespace != namespace || *deployment.Spec.Replicas != 3 || pod.Containers[0].Image != data["ControllerImage"] || pod.Containers[0].EnvFrom[0].ConfigMapRef.Name != config.Name {
				t.Fatal("controller configuration mismatch")
			}

			if pod.Volumes[0].Secret.SecretName != data["ServingTLSSecret"] || pod.Containers[0].VolumeMounts[0].MountPath+"/tls.crt" != cfg.TLSCertificateFile {
				t.Fatal("serving TLS not wired")
			}

			if cfg.ControlURL != "https://"+service.Name+"."+namespace+".svc:8443" || service.Spec.PublishNotReadyAddresses || pod.Containers[0].ReadinessProbe.HTTPGet.Path != "/readyz" {
				t.Fatal("service must select ready leader")
			}

			var (
				controllerSA, dataplaneSA corev1.ServiceAccount
				clusterRole               rbacv1.ClusterRole
				clusterBinding            rbacv1.ClusterRoleBinding
				role                      rbacv1.Role
				binding                   rbacv1.RoleBinding
			)

			decode("rbac.yaml", &controllerSA, &dataplaneSA, &clusterRole, &clusterBinding, &role, &binding)

			if dataplaneSA.Namespace != namespace || dataplaneSA.Name != ds.Spec.Template.Spec.ServiceAccountName || dataplaneSA.AutomountServiceAccountToken == nil || *dataplaneSA.AutomountServiceAccountToken {
				t.Fatal("dataplane service account mismatch")
			}

			if binding.Subjects[0].Name != controllerSA.Name || binding.Subjects[0].Namespace != namespace || clusterBinding.Subjects[0].Name != controllerSA.Name {
				t.Fatal("controller RBAC binding mismatch")
			}

			grants := func(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
				for _, rule := range rules {
					if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) {
						return true
					}
				}

				return false
			}
			for _, verb := range []string{"get", "list", "watch", "create", "patch"} {
				if !grants(role.Rules, "apps", "daemonsets", verb) {
					t.Fatalf("missing workload permission %s", verb)
				}
			}

			if !grants(clusterRole.Rules, "authentication.k8s.io", "tokenreviews", "create") {
				t.Fatal("missing bootstrap permission")
			}
		})
	}
}

func TestBootstrapTrustCanBeProvisionedExternally(t *testing.T) {
	out := t.TempDir()
	if err := render.Render(".", out, map[string]string{}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(out, "bootstrap-trust.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var object corev1.ConfigMap
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096).Decode(&object); (err != nil && err != io.EOF) || object.Name != "" || object.Kind != "" || object.Data != nil {
		t.Fatalf("default render must not replace external trust: %v", err)
	}
}
