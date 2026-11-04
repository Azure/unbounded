// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

// Media artifacts and first-boot templates are pulled at request time from
// the frozen image references with the frozen pull secrets, even after the
// live Machine switched to other images and secrets.
func TestMediaFrozenImagesUseFrozenPullSecrets(t *testing.T) {
	p, m, op, images := mediaFixtureImages(t, "amd64", map[string][]byte{
		"vmlinuz":                     []byte("frozen kernel"),
		"cloud-init/meta-data.tmpl":   []byte("frozen {{ .Machine.Name }} {{ .BootLease.IPv4 }}"),
		"cloud-init/vendor-data.tmpl": []byte("frozen vendor-data"),
	}, map[string][]byte{"disk.img.gz": []byte("frozen disk")}, imagepulltest.WithBasicAuth("user", "pass"))

	frozenSecret := dockerConfigJSONSecret("frozen", `{"auths":{"test":{"username":"user","password":"pass"}}}`)
	frozenSecret.Namespace = "images"
	liveSecret := dockerConfigJSONSecret("live", `{"auths":{"test":{"username":"user","password":"wrong"}}}`)
	liveSecret.Namespace = "images"

	b := op.Status.Targets[0].BootMedia
	b.Netboot.PullSecretRef = secretRef("images", "frozen")
	b.Netboot.NetbootPullSecretRef = secretRef("images", "frozen")
	frozen := b.Netboot.DeepCopy()

	// The live Machine now names other images and a secret that the
	// registry rejects.
	m.Spec.Netboot().Image = images.Push(t, "test/edited-disk:v1", map[string][]byte{"disk.img.gz": []byte("edited disk")})
	m.Spec.Netboot().NetbootImage = images.Push(t, "test/edited-netboot:v1", map[string][]byte{"vmlinuz": []byte("edited kernel")})
	m.Spec.Netboot().PullSecretRef = secretRef("images", "live")
	m.Spec.Netboot().NetbootPullSecretRef = secretRef("images", "live")

	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(op).WithObjects(m, op, frozenSecret, liveSecret).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
	p.Client, p.Resolver.Reader = fc, fc
	p.Resolver.Cluster = &StaticClusterInfo{}

	var calls []string

	p.Run = mockMediaRunner(t, &calls)
	_, err := p.Prepare(t.Context(), m, op, b.ID)
	require.NoError(t, err, "the ISO build pulls EFI payloads with the frozen secret")

	h := &HTTPServer{Media: p, FileResolver: *p.Resolver}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "192.0.2.20:4321"
		w := httptest.NewRecorder()
		normalizePath(http.HandlerFunc(h.handleFile)).ServeHTTP(w, r)

		return w
	}

	for path, want := range map[string]string{"/disk.img.gz": "frozen disk", "/vmlinuz": "frozen kernel"} {
		w := get(path)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, want, w.Body.String())
	}

	// Without the frozen secret the registry rejects the pull; the artifact
	// is unavailable rather than served from a stale cache.
	require.NoError(t, fc.Delete(t.Context(), frozenSecret))

	w := get("/vmlinuz")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "5", w.Header().Get("Retry-After"))

	frozenSecret.ResourceVersion = ""
	require.NoError(t, fc.Create(t.Context(), frozenSecret))
	require.Equal(t, http.StatusOK, get("/vmlinuz").Code)

	// After handoff, first-boot templates still come from the frozen
	// netboot image.
	require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(op), op))

	now := metav1.Now()
	b = op.Status.Targets[0].BootMedia
	b.ImageWritten, b.Cleaned, b.Released = true, true, true
	b.RebootRequestedAt = &now
	op.Status.Phase = v1alpha3.OperationPhaseFailed
	require.NoError(t, fc.Status().Update(t.Context(), op))

	w = get("/cloud-init/meta-data")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "frozen host 192.0.2.20", w.Body.String())

	require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(op), op))
	require.Equal(t, frozen, op.Status.Targets[0].BootMedia.Netboot)
}
