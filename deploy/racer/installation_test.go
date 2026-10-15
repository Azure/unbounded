// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestInstallationRenderingRequiresExplicitFreshUUID(t *testing.T) {
	const cluster = "11111111-1111-1111-1111-111111111111"

	for _, tc := range []struct {
		name, state, cluster string
		wantMarker, wantErr  bool
	}{
		{name: "defaults"},
		{name: "configured", cluster: cluster},
		{name: "consumed", state: "consumed", cluster: cluster},
		{name: "not-explicit", state: "Fresh", cluster: cluster},
		{name: "fresh", state: "fresh", cluster: cluster, wantMarker: true},
		{name: "missing-uuid", state: "fresh", wantErr: true},
		{name: "invalid-uuid", state: "fresh", cluster: "old-cluster", wantErr: true},
		{name: "zero-uuid", state: "fresh", cluster: "00000000-0000-0000-0000-000000000000", wantErr: true},
		{name: "uppercase-uuid", state: "fresh", cluster: "AAAAAAAA-1111-1111-1111-111111111111", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()

			err := render.Render(".", out, map[string]string{"InitializationState": tc.state, "ClusterID": tc.cluster})
			if tc.wantErr {
				require.ErrorContains(t, err, "fresh installation requires a new, nonzero canonical ClusterID UUID")
				return
			}

			require.NoError(t, err)
			b, err := os.ReadFile(filepath.Join(out, "installation.yaml"))
			require.NoError(t, err)

			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)

			var marker corev1.ConfigMap

			err = decoder.Decode(&marker)
			if !tc.wantMarker {
				require.True(t, err == nil || err == io.EOF, "%v", err)
				require.Empty(t, marker.Name)
				require.Empty(t, marker.Kind)
				require.Nil(t, marker.Data)

				return
			}

			require.NoError(t, err)
			require.Equal(t, "ConfigMap", marker.Kind)
			require.Equal(t, "racer-installation", marker.Name)
			require.Equal(t, map[string]string{"cluster": cluster, "version_configmap": "racer-version", "state": "fresh", "initialization_protocol": "staged-v1"}, marker.Data)
			require.NotNil(t, marker.Immutable)
			require.False(t, *marker.Immutable)
			require.ErrorIs(t, decoder.Decode(&corev1.ConfigMap{}), io.EOF)
		})
	}
}

func TestDefaultRenderHasNoInstallationApplyOperations(t *testing.T) {
	out := t.TempDir()
	data := map[string]string{"InitializationState": "fresh", "ClusterID": "11111111-1111-1111-1111-111111111111"}
	require.NoError(t, render.Render(".", out, data))

	env := &component.Env{Namespace: "unbounded-system"}
	objects, err := env.DecodeManifestFiles(os.DirFS(out), []string{"installation.yaml"}, nil)
	require.NoError(t, err)
	require.Len(t, objects, 1)

	// Reuse the output directory so a previous fresh manifest cannot survive a
	// normal render and accidentally enter an apply plan.
	delete(data, "InitializationState")
	require.NoError(t, render.Render(".", out, data))
	objects, err = env.DecodeManifestFS(os.DirFS(out), nil)
	require.NoError(t, err)

	for _, operation := range component.ApplyOperations(objects, "racer", "") {
		if operation.Object.GetKind() == "ConfigMap" {
			require.NotContains(t, []string{"racer-installation", "racer-version"}, operation.Object.GetName())
		}

		require.NotEqual(t, "Secret", operation.Object.GetKind(), "rendering must not recreate private authority")
	}
}

func TestEnvtestDefaultApplyPreservesFinalizedInstallation(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server apply assertions")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	c, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	ctx := t.Context()

	const namespace = "unbounded-system"
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	env := &component.Env{Client: c, Scheme: scheme.Scheme, Namespace: namespace}
	out := t.TempDir()
	data := map[string]string{"InitializationState": "fresh", "ClusterID": "11111111-1111-1111-1111-111111111111"}
	require.NoError(t, render.Render(".", out, data))
	objects, err := env.DecodeManifestFiles(os.DirFS(out), []string{"installation.yaml"}, nil)
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.NoError(t, c.Create(ctx, objects[0]))

	key := client.ObjectKey{Namespace: namespace, Name: "racer-installation"}
	marker := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, key, marker))
	marker.Data["state"] = "consumed"
	marker.Data["version_uid"] = "22222222-2222-2222-2222-222222222222"
	marker.Immutable = ptr.To(true)
	require.NoError(t, c.Update(ctx, marker))
	finalized := marker.DeepCopy()

	delete(data, "InitializationState")
	require.NoError(t, render.Render(".", out, data))
	objects, err = env.DecodeManifestFiles(os.DirFS(out), []string{"installation.yaml", "config.yaml", "controller.yaml", "controller-pdb.yaml"}, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		require.NoError(t, env.ApplyObject(ctx, obj))
	}

	require.NoError(t, c.Get(ctx, key, marker))
	require.Equal(t, finalized, marker, "default apply must leave the immutable marker, UID and commitments unchanged")

	budget := &policyv1.PodDisruptionBudget{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer-controller"}, budget))
	require.Equal(t, 2, budget.Spec.MinAvailable.IntValue())
}
