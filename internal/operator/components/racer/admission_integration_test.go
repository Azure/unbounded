// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// Use an isolated API server with no reconcilers so both allowed names are
// absent for every CREATE. AlreadyExists must never mask an admission verdict.
func TestEnvtestRuntimeSecretAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server admission")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	env := testEnv(t)
	env.Client, err = client.New(rc, client.Options{Scheme: env.Scheme})
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))
	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml"}, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		require.NoError(t, env.ApplyObject(ctx, obj))
	}

	username := "system:serviceaccount:" + env.Namespace + ":racer-controller"
	require.NoError(t, env.Client.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "admission-test", Namespace: env.Namespace},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "update", "patch"}}},
	}))
	require.NoError(t, env.Client.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admission-test", Namespace: env.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "admission-test"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: username}},
	}))

	restricted := rest.CopyConfig(rc)
	restricted.Impersonate = rest.ImpersonationConfig{UserName: username}
	c, err := client.New(restricted, client.Options{Scheme: env.Scheme})
	require.NoError(t, err)
	denied := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "Racer may only write")
	}

	require.Eventually(t, func() bool {
		return denied(c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "not-allowed", Namespace: env.Namespace}}, client.DryRunAll))
	}, 30*time.Second, 100*time.Millisecond)

	for _, entry := range []string{"issuer.json", "bundle.json", "rotation.json"} {
		t.Run(entry, func(t *testing.T) {
			secretName := "racer-credentials"

			for _, tc := range []struct {
				name        string
				typeName    corev1.SecretType
				annotations map[string]string
				allowed     bool
			}{
				{name: "opaque", typeName: corev1.SecretTypeOpaque, allowed: true},
				{name: "default-opaque", allowed: true},
				{name: "unrelated-annotation", typeName: corev1.SecretTypeOpaque, annotations: map[string]string{"example.test/note": "ok"}, allowed: true},
				{name: "token", typeName: corev1.SecretTypeServiceAccountToken, annotations: map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}},
				{name: "custom-type", typeName: "example.test/custom"},
				{name: "sa-name-empty", typeName: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountNameKey: ""}},
				{name: "sa-name", typeName: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}},
				{name: "sa-uid-empty", typeName: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountUIDKey: ""}},
				{name: "sa-uid", typeName: corev1.SecretTypeOpaque, annotations: map[string]string{corev1.ServiceAccountUIDKey: "uid"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: env.Namespace, Annotations: tc.annotations}, Type: tc.typeName, Data: map[string][]byte{entry: []byte("{}")}}
					require.True(t, apierrors.IsNotFound(env.Client.Get(ctx, objectKey(env, secretName), &corev1.Secret{})))

					err := c.Create(ctx, obj, client.DryRunAll)
					if tc.allowed {
						require.NoError(t, err)
					} else {
						require.True(t, denied(err), "CREATE: %v", err)
					}

					baseline := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: env.Namespace}, Type: tc.typeName}
					if baseline.Type == corev1.SecretTypeServiceAccountToken {
						baseline.Annotations = map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}
					}

					require.NoError(t, env.Client.Create(ctx, baseline))

					defer func() { require.NoError(t, env.Client.Delete(ctx, baseline)) }()

					updated := baseline.DeepCopy()

					updated.Annotations = tc.annotations
					for _, err := range []error{c.Update(ctx, updated, client.DryRunAll), c.Patch(ctx, updated, client.MergeFrom(baseline), client.DryRunAll)} {
						if tc.allowed {
							require.NoError(t, err)
						} else {
							require.True(t, denied(err), "UPDATE/PATCH: %v", err)
						}
					}
				})
			}
		})
	}
}

// Exercise the deployed Role, not an expanded test role: admission cannot protect
// reads, and resourceNames on LIST/WATCH requires an exact name field selector.
func TestEnvtestRuntimeRBAC(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server RBAC")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "custom-names"}[custom], func(t *testing.T) {
			env := testEnv(t)
			if custom {
				env.Namespace = "custom-names"
			}

			env.Client, err = client.New(rc, client.Options{Scheme: env.Scheme})
			require.NoError(t, err)
			ctx := t.Context()
			require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))

			credentials, installation, version := "racer-credentials", "racer-installation", "racer-version"
			data := map[string]string{"Namespace": env.Namespace}

			if custom {
				credentials, installation, version = "custom.credentials", "custom.installation", "custom.version"
				data["CredentialsSecretName"], data["InstallationConfigMapName"], data["VersionConfigMapName"] = credentials, installation, version
			}

			out := t.TempDir()
			require.NoError(t, render.Render("../../../../deploy/racer", out, data))
			objects, err := env.DecodeManifestFiles(os.DirFS(out), []string{"create-restriction.yaml", "rbac.yaml"}, nil)
			require.NoError(t, err)

			for _, obj := range objects {
				require.NoError(t, env.ApplyObject(ctx, obj))
			}

			restricted := rest.CopyConfig(rc)
			restricted.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + env.Namespace + ":racer-controller"}
			c, err := client.NewWithWatch(restricted, client.Options{Scheme: env.Scheme})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "not-allowed", Namespace: env.Namespace}}, client.DryRunAll)
				return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "Racer may only write")
			}, 30*time.Second, 100*time.Millisecond)

			for _, name := range []string{installation, version} {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Namespace}}
				require.NoError(t, c.Create(ctx, cm))
				before := cm.DeepCopy()
				cm.Data = map[string]string{"test": "updated"}
				require.NoError(t, c.Update(ctx, cm))
				require.NoError(t, c.Patch(ctx, cm, client.MergeFrom(before)))
			}

			for _, name := range []string{"unrelated", "racer-config"} {
				err := c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Namespace}}, client.DryRunAll)
				require.True(t, apierrors.IsForbidden(err), "%v", err)

				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Namespace}}
				require.NoError(t, env.Client.Create(ctx, cm))
				before := cm.DeepCopy()
				cm.Data = map[string]string{"test": "denied"}
				require.True(t, apierrors.IsForbidden(c.Update(ctx, cm)))
				require.True(t, apierrors.IsForbidden(c.Patch(ctx, cm, client.MergeFrom(before))))
			}

			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentials, Namespace: env.Namespace}, Type: corev1.SecretTypeOpaque}
			require.NoError(t, c.Create(ctx, secret))
			require.NoError(t, c.Get(ctx, objectKey(env, credentials), &corev1.Secret{}))

			before := secret.DeepCopy()
			secret.Data = map[string][]byte{"issuer.json": []byte("test")}
			require.NoError(t, c.Update(ctx, secret))
			require.NoError(t, c.Patch(ctx, secret, client.MergeFrom(before)))

			for _, name := range []string{"racer-controller-tls", "unrelated"} {
				unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Namespace}, Data: map[string][]byte{"ca.key": []byte("must-not-be-readable")}}
				require.NoError(t, env.Client.Create(ctx, unrelated))
				require.True(t, apierrors.IsForbidden(c.Get(ctx, objectKey(env, name), &corev1.Secret{})))

				before := unrelated.DeepCopy()
				unrelated.Data["ca.key"] = []byte("denied")
				require.True(t, apierrors.IsForbidden(c.Update(ctx, unrelated)))
				require.True(t, apierrors.IsForbidden(c.Patch(ctx, unrelated, client.MergeFrom(before))))
			}

			for _, name := range []string{credentials, "racer-controller-tls", "unrelated", ""} {
				opts := []client.ListOption{client.InNamespace(env.Namespace)}
				if name != "" {
					opts = append(opts, client.MatchingFields{"metadata.name": name})
				}

				list := &corev1.SecretList{}
				listErr := c.List(ctx, list, opts...)

				watch, watchErr := c.Watch(ctx, &corev1.SecretList{}, opts...)
				if watch != nil {
					watch.Stop()
				}

				if name == credentials {
					require.NoError(t, listErr)
					require.NoError(t, watchErr)
					require.Len(t, list.Items, 1)
					require.Equal(t, credentials, list.Items[0].Name)
				} else {
					require.True(t, apierrors.IsForbidden(listErr), "list %q: %v", name, listErr)
					require.True(t, apierrors.IsForbidden(watchErr), "watch %q: %v", name, watchErr)
				}
			}
		})
	}
}
