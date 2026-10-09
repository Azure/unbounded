// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestEnvtestRenderedRuntimePolicy(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server runtime admission assertions")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "custom-namespace-and-names"}[custom], func(t *testing.T) {
			ctx := t.Context()
			namespace, credentials, installation, version := "unbounded-system", "racer-credentials", "racer-installation", "racer-version"
			data := map[string]string{}

			if custom {
				namespace, credentials, installation, version = "custom-racer", "custom.credentials", "custom.installation", "custom.version"
				data = map[string]string{"Namespace": namespace, "CredentialsSecretName": credentials, "InstallationConfigMapName": installation, "VersionConfigMapName": version}
			}

			out := t.TempDir()
			require.NoError(t, render.Render(".", out, data))
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

			env := &component.Env{Namespace: namespace}
			objects, err := env.DecodeManifestFiles(os.DirFS(out), []string{"create-restriction.yaml", "rbac.yaml"}, nil)
			require.NoError(t, err)

			for _, obj := range objects {
				require.NoError(t, admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("racer-runtime-policy-test")))
			}

			username := "system:serviceaccount:" + namespace + ":racer-controller"
			asUser := func(username string) client.Client {
				t.Helper()

				config := rest.CopyConfig(rc)
				config.Impersonate = rest.ImpersonationConfig{UserName: username}
				c, err := client.New(config, client.Options{Scheme: scheme.Scheme})
				require.NoError(t, err)

				return c
			}
			controller := asUser(username)
			configMap := func(name string) *corev1.ConfigMap {
				return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			}
			secret := func(name string) *corev1.Secret {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeOpaque}
			}

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				// Envtest has no policy status controller. Execute both resource types
				// and both verdicts so CEL compilation errors cannot pass as denials.
				require.NoError(c, controller.Create(ctx, configMap(installation), client.DryRunAll))
				require.NoError(c, controller.Create(ctx, secret(credentials), client.DryRunAll))
				requireRuntimePolicyDenied(c, controller.Create(ctx, configMap("unrelated"), client.DryRunAll))
				requireRuntimePolicyDenied(c, controller.Create(ctx, secret("unrelated"), client.DryRunAll))
			}, 15*time.Second, 100*time.Millisecond)

			// Prove the allowed runtime writes with the shipped RBAC first.
			for _, obj := range []client.Object{configMap(installation), configMap(version), secret(credentials)} {
				t.Run("allowed-"+obj.GetName(), func(t *testing.T) {
					testRuntimePolicyWrites(t, admin, controller, obj, true)
				})
			}

			// Broader test-only grants prevent RBAC from masking name and namespace
			// admission failures on UPDATE/PATCH, or excluding the other identities.
			otherNamespace := namespace + "-other"
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNamespace}}))
			users := []string{username, "system:serviceaccount:" + otherNamespace + ":racer-controller", "system:serviceaccount:" + namespace + ":racer-dataplane"}

			subjects := make([]rbacv1.Subject, 0, len(users))
			for _, user := range users {
				subjects = append(subjects, rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: user})
			}

			for _, ns := range []string{namespace, otherNamespace} {
				role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "runtime-policy-test", Namespace: ns}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: []string{"get", "create", "update", "patch"}}}}
				require.NoError(t, admin.Create(ctx, role))
				require.NoError(t, admin.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: ns}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: subjects}))
			}

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				for _, user := range users {
					for _, ns := range []string{namespace, otherNamespace} {
						err := asUser(user).Get(ctx, client.ObjectKey{Namespace: ns, Name: "rbac-ready"}, &corev1.Secret{})
						require.True(c, apierrors.IsNotFound(err), "%s in %s: %v", user, ns, err)
					}
				}
			}, 10*time.Second, 100*time.Millisecond)

			for _, name := range []string{"unrelated", "racer-config", credentials} {
				t.Run("deny-configmap-"+name, func(t *testing.T) {
					testRuntimePolicyWrites(t, admin, controller, configMap(name), false)
				})
			}

			for _, name := range []string{"unrelated", "racer-controller-tls", installation, version} {
				t.Run("deny-secret-"+name, func(t *testing.T) {
					testRuntimePolicyWrites(t, admin, controller, secret(name), false)
				})
			}

			if custom {
				for _, obj := range []client.Object{configMap("racer-installation"), configMap("racer-version"), secret("racer-credentials")} {
					t.Run("deny-default-name-"+obj.GetName(), func(t *testing.T) {
						testRuntimePolicyWrites(t, admin, controller, obj, false)
					})
				}
			}

			for _, tc := range []struct {
				name        string
				secretType  corev1.SecretType
				annotations map[string]string
				allowed     bool
			}{
				{name: "default-opaque", allowed: true},
				{name: "empty-annotations", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{}, allowed: true},
				{name: "runtime-claim", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{"racer.unbounded-cloud.io/credentials": "candidate"}, allowed: true},
				{name: "other-annotation", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{"example.test/note": "ok"}, allowed: true},
				{name: "token", secretType: corev1.SecretTypeServiceAccountToken, annotations: map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}},
				{name: "custom-type", secretType: "example.test/custom"},
				{name: "sa-name-empty", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountNameKey: ""}},
				{name: "sa-name", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}},
				{name: "sa-uid-empty", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountUIDKey: ""}},
				{name: "sa-uid", secretType: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountUIDKey: "uid"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					obj := secret(credentials)
					obj.Type, obj.Annotations = tc.secretType, tc.annotations
					testRuntimePolicyWrites(t, admin, controller, obj, tc.allowed)
				})
			}

			for _, obj := range []client.Object{configMap(installation), configMap(version), secret(credentials)} {
				obj.SetNamespace(otherNamespace)
				t.Run("deny-other-namespace-"+obj.GetName(), func(t *testing.T) {
					testRuntimePolicyWrites(t, admin, controller, obj, false)
				})
			}

			for _, user := range users[1:] {
				t.Run("unrestricted-"+user, func(t *testing.T) {
					for _, obj := range []client.Object{configMap("unrelated"), secret("unrelated")} {
						testRuntimePolicyWrites(t, admin, asUser(user), obj, true)
					}
				})
			}
		})
	}
}

func requireRuntimePolicyDenied(t require.TestingT, err error) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}

	require.True(t, apierrors.IsForbidden(err), "%v", err)
	require.ErrorContains(t, err, "racer-runtime-write-restriction", "must be denied by admission, not RBAC")
	require.ErrorContains(t, err, "Racer may only write", "must fail validation, not CEL evaluation")
}

func testRuntimePolicyWrites(t *testing.T, admin, controller client.Client, obj client.Object, allowed bool) {
	t.Helper()

	ctx := t.Context()
	check := func(err error) {
		t.Helper()

		if allowed {
			require.NoError(t, err)
		} else {
			requireRuntimePolicyDenied(t, err)
		}
	}

	// Keep the name absent for CREATE so AlreadyExists cannot mask admission.
	baseline := obj.DeepCopyObject().(client.Object)
	require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(obj), baseline)))
	check(controller.Create(ctx, obj.DeepCopyObject().(client.Object), client.DryRunAll))

	// Secret types are immutable. Seed the same type as admin to isolate admission
	// on updates of existing non-Opaque Secrets, and add annotations to clean ones.
	baseline.SetAnnotations(nil)

	if secret, ok := baseline.(*corev1.Secret); ok && secret.Type == corev1.SecretTypeServiceAccountToken {
		baseline.SetAnnotations(map[string]string{corev1.ServiceAccountNameKey: "racer-controller"})
	}

	require.NoError(t, admin.Create(ctx, baseline))

	defer func() { require.NoError(t, admin.Delete(ctx, baseline)) }()

	updated := baseline.DeepCopyObject().(client.Object)
	updated.SetAnnotations(obj.GetAnnotations())

	switch obj := updated.(type) {
	case *corev1.ConfigMap:
		obj.Data = map[string]string{"test": "updated"}
	case *corev1.Secret:
		obj.Data = map[string][]byte{"issuer.json": []byte("{}"), "bundle.json": []byte("{}"), "rotation.json": []byte("{}")}
	}

	check(controller.Update(ctx, updated.DeepCopyObject().(client.Object), client.DryRunAll))
	check(controller.Patch(ctx, updated, client.MergeFrom(baseline), client.DryRunAll))
}
