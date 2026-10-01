// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

			var config, trust, dataplaneConfig corev1.ConfigMap
			decode("config.yaml", &config)
			decode("bootstrap-trust.yaml", &trust)
			decode("dataplane-config.yaml", &dataplaneConfig)

			if dataplaneConfig.Namespace != namespace || dataplaneConfig.Name != "racer-dataplane-config" || dataplaneConfig.Data["RACER_REQUEST_CONTEXT_BYTES"] != "67108864" {
				t.Fatal("dataplane defaults must fund the Rust peer relay progress floor at four workers")
			}

			if _, capped := dataplaneConfig.Data["RACER_MAX_THREADS"]; capped {
				t.Fatal("dataplane defaults must leave worker sizing automatic")
			}

			for key, value := range config.Data {
				t.Setenv(key, value)
			}

			t.Setenv("POD_NAMESPACE", namespace)

			cfg, err := racer.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}

			workloadCfg, err := racer.WorkloadConfigFromLookup(os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}

			ds, err := racer.DesiredDaemonSet(workloadCfg)
			if err != nil {
				t.Fatal(err)
			}

			if ds.Namespace != namespace || ds.Spec.Template.Spec.Containers[0].Image != data["DataplaneImage"] {
				t.Fatal("workload config not wired")
			}

			volumes := map[string]corev1.Volume{}

			for _, volume := range ds.Spec.Template.Spec.Volumes {
				if volume.Secret != nil || volume.Name == "keyring" {
					t.Fatal("dataplane shared keys must be delivered through control HTTPS")
				}

				volumes[volume.Name] = volume
			}

			if volumes["token"].Projected.Sources[0].ServiceAccountToken.Audience != "racer-control" || volumes["bootstrap"].ConfigMap.Name != workloadCfg.BootstrapTrustConfigMap {
				t.Fatal("bootstrap token and controller trust must be retained")
			}

			for _, name := range []string{"identity", "slabs", "sockets"} {
				if volumes[name].HostPath == nil {
					t.Fatalf("node-local %s mount missing", name)
				}
			}

			for _, variable := range ds.Spec.Template.Spec.Containers[0].Env {
				if variable.Name == "RACER_SECRET_DIRECTORY" {
					t.Fatal("obsolete keyring directory must not be configured")
				}
			}

			if cfg.CredentialsSecretName != "racer-credentials" {
				t.Fatal("controller atomic credentials Secret configuration missing")
			}

			if trust.Namespace != namespace || trust.Name != workloadCfg.BootstrapTrustConfigMap || trust.Data["ca.crt"] != data["BootstrapCA"] {
				t.Fatal("deployment trust not wired")
			}

			var (
				deployment appsv1.Deployment
				service    corev1.Service
			)

			decode("controller.yaml", &deployment, &service)

			budget := deployment.Spec.Strategy.RollingUpdate
			if deployment.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || budget == nil || budget.MaxUnavailable == nil || budget.MaxSurge == nil || budget.MaxUnavailable.IntValue() != 0 || budget.MaxSurge.IntValue() != 1 {
				t.Fatalf("controller rollout must preserve serving replicas with one surge: %+v", deployment.Spec.Strategy)
			}

			pod := deployment.Spec.Template.Spec
			if deployment.Namespace != namespace || *deployment.Spec.Replicas != 3 || pod.Containers[0].Image != data["ControllerImage"] || pod.Containers[0].EnvFrom[0].ConfigMapRef.Name != config.Name {
				t.Fatal("controller configuration mismatch")
			}

			if pod.Volumes[0].Secret.SecretName != data["ServingTLSSecret"] || pod.Containers[0].VolumeMounts[0].MountPath+"/tls.crt" != cfg.TLSCertificateFile {
				t.Fatal("serving TLS not wired")
			}

			if !slices.Equal(pod.Volumes[0].Secret.Items, []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca-bundle.crt", Path: "ca.crt"}}) || pod.Containers[0].VolumeMounts[0].MountPath+"/ca.crt" != cfg.ReplicationTrustFile {
				t.Fatal("replication requires current and retained serving CAs, never their private keys")
			}

			if cfg.ControllerServiceAccount != pod.ServiceAccountName || cfg.ReplicationServerName != service.Name+"."+namespace+".svc" || cfg.ReplicationPort != 8443 || cfg.SnapshotMaxAge != 30*time.Second {
				t.Fatal("replication identity, TLS name, port, or freshness not wired")
			}

			projection := pod.Volumes[1].Projected
			if projection == nil || projection.DefaultMode == nil || *projection.DefaultMode != 0o400 || len(projection.Sources) != 1 {
				t.Fatal("replication requires a dedicated protected token projection")
			}

			token := projection.Sources[0].ServiceAccountToken

			mount := pod.Containers[0].VolumeMounts[1]
			if token == nil || token.Audience != racer.ReplicationAudience || token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 || mount.Name != pod.Volumes[1].Name || !mount.ReadOnly || mount.SubPath != "" || mount.MountPath+"/"+token.Path != cfg.ReplicationTokenFile {
				t.Fatal("rotating replication token must use its dedicated audience and configured path")
			}

			for name, field := range map[string]string{"POD_NAMESPACE": "metadata.namespace", "POD_NAME": "metadata.name", "POD_UID": "metadata.uid"} {
				if !slices.ContainsFunc(pod.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == name && env.ValueFrom != nil && env.ValueFrom.FieldRef != nil && env.ValueFrom.FieldRef.FieldPath == field
				}) {
					t.Fatalf("missing downward API identity %s", name)
				}
			}

			if workloadCfg.ControlURL != "https://"+service.Name+"."+namespace+".svc:8443" || service.Spec.PublishNotReadyAddresses || pod.Containers[0].ReadinessProbe.HTTPGet.Path != "/readyz" {
				t.Fatal("service must select every synchronized ready replica")
			}

			if len(service.Spec.Selector) == 0 || !maps.Equal(service.Spec.Selector, deployment.Spec.Template.Labels) {
				t.Fatal("service must route to controller pods through readiness filtering")
			}

			controller := pod.Containers[0]
			if controller.ReadinessProbe.HTTPGet.Port.StrVal != "probes" || controller.LivenessProbe.HTTPGet.Path != "/healthz" || controller.LivenessProbe.HTTPGet.Port.StrVal != "probes" {
				t.Fatal("unsynchronized replicas must remain live while readiness gates service routing")
			}

			if !slices.ContainsFunc(controller.Ports, func(port corev1.ContainerPort) bool {
				return port.Name == "probes" && port.ContainerPort == 8081
			}) {
				t.Fatal("replica readiness probe port must reach the controller probe listener")
			}

			var (
				controllerSA, dataplaneSA corev1.ServiceAccount
				clusterRole               rbacv1.ClusterRole
				clusterBinding            rbacv1.ClusterRoleBinding
				role                      rbacv1.Role
				binding                   rbacv1.RoleBinding
			)

			decode("rbac.yaml", &controllerSA, &dataplaneSA, &clusterRole, &clusterBinding, &role, &binding)

			credentialWrites := false

			for _, rule := range role.Rules {
				if slices.Contains(rule.Resources, "secrets") && (slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "patch")) {
					if !slices.Equal(rule.ResourceNames, []string{cfg.CredentialsSecretName}) {
						t.Fatal("credential update RBAC must name only the atomic Secret")
					}

					credentialWrites = true
				}
			}

			if !credentialWrites {
				t.Fatal("credential CAS RBAC missing")
			}

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
			for _, verb := range []string{"get", "list", "watch"} {
				if !grants(role.Rules, "apps", "daemonsets", verb) {
					t.Fatalf("missing workload permission %s", verb)
				}
			}

			for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
				if grants(role.Rules, "apps", "daemonsets", verb) {
					t.Fatalf("controller may not mutate workloads: %s", verb)
				}
			}

			if !grants(clusterRole.Rules, "authentication.k8s.io", "tokenreviews", "create") {
				t.Fatal("missing bootstrap and replication authentication permission")
			}

			for _, resource := range []string{"pods", "serviceaccounts"} {
				if !grants(role.Rules, "", resource, "get") {
					t.Fatalf("missing live replication identity read: %s", resource)
				}
			}

			if !grants(role.Rules, "coordination.k8s.io", "leases", "get") {
				t.Fatal("missing direct publisher discovery permission")
			}
		})
	}
}

func TestRenderedControllerPreservesServingReplicasDuringRollout(t *testing.T) {
	out := t.TempDir()
	if err := render.Render(".", out, map[string]string{}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(out, "controller.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var deployment unstructured.Unstructured
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096).Decode(&deployment); err != nil {
		t.Fatal(err)
	}

	for field, want := range map[string]int64{"maxUnavailable": 0, "maxSurge": 1} {
		value, found, err := unstructured.NestedInt64(deployment.Object, "spec", "strategy", "rollingUpdate", field)
		if err != nil || !found || value != want {
			t.Fatalf("rollout %s must be explicit: value=%v want=%v found=%t err=%v", field, value, want, found, err)
		}
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
