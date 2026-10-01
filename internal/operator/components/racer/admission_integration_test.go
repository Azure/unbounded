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

	for _, secretName := range []string{"racer-issuer", "racer-keyring"} {
		t.Run(secretName, func(t *testing.T) {
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
					obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: env.Namespace, Annotations: tc.annotations}, Type: tc.typeName}
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
