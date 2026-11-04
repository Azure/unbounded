// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

func TestImageSources(t *testing.T) {
	tests := []struct {
		name        string
		resolver    *FileResolver
		machine     *v1alpha3.Machine
		wantDisk    ImageSource
		wantNetboot ImageSource
	}{
		{
			name:     "explicit netboot image",
			resolver: &FileResolver{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/machine:v1",
				NetbootImage: "ghcr.io/test/netboot:v1",
			}}},
			wantDisk:    ImageSource{Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture},
			wantNetboot: ImageSource{Ref: "ghcr.io/test/netboot:v1", Architecture: v1alpha3.DefaultPXEArchitecture},
		},
		{
			name:     "default netboot image",
			resolver: &FileResolver{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image: "ghcr.io/test/machine:v1",
			}}},
			wantDisk:    ImageSource{Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture},
			wantNetboot: ImageSource{Ref: "ghcr.io/test/default-netboot:v1", Architecture: v1alpha3.DefaultPXEArchitecture},
		},
		{
			name: "pull secrets follow the selected image",
			resolver: &FileResolver{
				DefaultNetbootRef:           "ghcr.io/test/default-netboot:v1",
				DefaultNetbootPullSecretRef: secretRef("unbounded-kube", "default-netboot"),
			},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image:         "ghcr.io/test/machine:v1",
				PullSecretRef: secretRef("tenant-a", "machine-image"),
			}}},
			wantDisk: ImageSource{
				Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture,
				PullSecretRef: secretRef("tenant-a", "machine-image"),
			},
			wantNetboot: ImageSource{
				Ref: "ghcr.io/test/default-netboot:v1", Architecture: v1alpha3.DefaultPXEArchitecture,
				PullSecretRef: secretRef("unbounded-kube", "default-netboot"),
			},
		},
		{
			name: "explicit netboot pull secret",
			resolver: &FileResolver{
				DefaultNetbootRef:           "ghcr.io/test/default-netboot:v1",
				DefaultNetbootPullSecretRef: secretRef("unbounded-kube", "default-netboot"),
			},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image:                "ghcr.io/test/machine:v1",
				NetbootImage:         "ghcr.io/test/netboot:v1",
				NetbootPullSecretRef: secretRef("tenant-a", "netboot-image"),
			}}},
			wantDisk: ImageSource{Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture},
			wantNetboot: ImageSource{
				Ref: "ghcr.io/test/netboot:v1", Architecture: v1alpha3.DefaultPXEArchitecture,
				PullSecretRef: secretRef("tenant-a", "netboot-image"),
			},
		},
		{
			name:     "machine image without default",
			resolver: &FileResolver{},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image:         "ghcr.io/test/machine:v1",
				PullSecretRef: secretRef("tenant-a", "machine-image"),
			}}},
			wantDisk: ImageSource{
				Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture,
				PullSecretRef: secretRef("tenant-a", "machine-image"),
			},
			wantNetboot: ImageSource{
				Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.DefaultPXEArchitecture,
				PullSecretRef: secretRef("tenant-a", "machine-image"),
			},
		},
		{
			name:     "arm64 architecture",
			resolver: &FileResolver{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/machine:v1",
				Architecture: v1alpha3.PXEArchitectureARM64,
			}}},
			wantDisk:    ImageSource{Ref: "ghcr.io/test/machine:v1", Architecture: v1alpha3.PXEArchitectureARM64},
			wantNetboot: ImageSource{Ref: "ghcr.io/test/default-netboot:v1", Architecture: v1alpha3.PXEArchitectureARM64},
		},
		{
			name:     "no pxe",
			resolver: &FileResolver{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine:  &v1alpha3.Machine{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantDisk, DiskImageSource(tt.machine))
			require.Equal(t, tt.wantNetboot, tt.resolver.NetbootImageSource(tt.machine))
		})
	}
}

// fileServerFixture serves files from a single netboot image to the Machine
// with IP 10.0.40.10 through handleFile.
type fileServerFixture struct {
	images  *testImages
	server  *HTTPServer
	machine *v1alpha3.Machine
	client  client.Client
}

const fixtureIP = "10.0.40.10"

func newFileServerFixture(t *testing.T, files map[string][]byte, opts ...imagepulltest.Option) *fileServerFixture {
	t.Helper()

	images := newTestImages(t, opts...)
	images.Push(t, "ghcr.io/test/netboot:v1", files)

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-node"},
		Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
			Image:        "ghcr.io/test/netboot:v1",
			NetbootImage: "ghcr.io/test/netboot:v1",
			DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:40", IPv4: fixtureIP, SubnetMask: "255.255.255.0"}},
		}},
	}

	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(machine).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()

	return &fileServerFixture{
		images:  images,
		machine: machine,
		client:  fc,
		server: &HTTPServer{FileResolver: FileResolver{
			Puller: images.Puller, Reader: fc, Cluster: &StaticClusterInfo{},
		}},
	}
}

func (f *fileServerFixture) get(path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = fixtureIP + ":4321"

	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}

	w := httptest.NewRecorder()
	normalizePath(http.HandlerFunc(f.server.handleFile)).ServeHTTP(w, r)

	return w
}

func bigPayload(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte('a' + i%26)
	}

	return data
}

func TestHTTPServerServesStaticFileWithRange(t *testing.T) {
	kernel := bigPayload(64 << 10)
	f := newFileServerFixture(t, map[string][]byte{"vmlinuz": kernel})

	w := f.get("/vmlinuz")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, strconv.Itoa(len(kernel)), w.Header().Get("Content-Length"))
	require.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
	require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	require.Equal(t, kernel, w.Body.Bytes())

	w = f.get("/vmlinuz", "Range", "bytes=10-19")
	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, fmt.Sprintf("bytes 10-19/%d", len(kernel)), w.Header().Get("Content-Range"))
	require.Equal(t, "10", w.Header().Get("Content-Length"))
	require.Equal(t, kernel[10:20], w.Body.Bytes())

	// A suffix range forces a seek to the end of the file.
	w = f.get("/vmlinuz", "Range", "bytes=-5")
	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, kernel[len(kernel)-5:], w.Body.Bytes())
}

func TestHTTPServerPullsFileForEveryRequest(t *testing.T) {
	f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")})
	f.images.Registry.ResetRequests()

	for range 2 {
		w := f.get("/vmlinuz")
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "kernel", w.Body.String())
	}

	// Image content is never cached: each request fetches the manifest
	// and the layer again. The puller may read a layer more than once per
	// request (for example to build its layer index), so only a floor is
	// asserted for blobs.
	require.Equal(t, 2, f.images.Registry.Count(imagepulltest.KindManifest))
	require.GreaterOrEqual(t, f.images.Registry.Count(imagepulltest.KindBlob), 2)
}

func TestHTTPServerFileErrors(t *testing.T) {
	t.Run("absent file", func(t *testing.T) {
		f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")})
		require.Equal(t, http.StatusNotFound, f.get("/initrd").Code)
	})

	t.Run("unreachable registry", func(t *testing.T) {
		f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")})
		f.server.Puller = unreachablePuller(t)

		w := f.get("/vmlinuz")
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.Equal(t, "5", w.Header().Get("Retry-After"))
	})

	t.Run("registry error", func(t *testing.T) {
		f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")},
			imagepulltest.WithMiddleware(func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "unavailable", http.StatusInternalServerError)
				})
			}))

		w := f.get("/vmlinuz")
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.Equal(t, "5", w.Header().Get("Retry-After"))
	})
}

func TestHTTPServerTemplatePrecedence(t *testing.T) {
	t.Run("template only", func(t *testing.T) {
		f := newFileServerFixture(t, map[string][]byte{
			"grub.cfg.tmpl": []byte("rendered for {{ .Machine.Name }}"),
		})

		w := f.get("/grub.cfg")
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "rendered for pull-node", w.Body.String())
	})

	for name, tc := range map[string]struct {
		lower, upper map[string][]byte
		want         string
	}{
		"upper template hides lower static file": {
			lower: map[string][]byte{"grub.cfg": []byte("static config")},
			upper: map[string][]byte{"grub.cfg.tmpl": []byte("rendered for {{ .Machine.Name }}")},
			want:  "rendered for pull-node",
		},
		"upper static file hides lower template": {
			lower: map[string][]byte{"grub.cfg.tmpl": []byte("rendered for {{ .Machine.Name }}")},
			upper: map[string][]byte{"grub.cfg": []byte("static config")},
			want:  "static config",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")})
			f.images.PushLayers(t, "ghcr.io/test/netboot:v1", tc.lower, tc.upper)

			w := f.get("/grub.cfg")
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tc.want, w.Body.String())
		})
	}
}

func TestHTTPServerUsesPullSecret(t *testing.T) {
	f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("private kernel")},
		imagepulltest.WithBasicAuth("user", "pass"))
	f.machine.Spec.PXE.NetbootPullSecretRef = secretRef("tenant-a", "netboot-pull")
	require.NoError(t, f.client.Update(t.Context(), f.machine))

	// The Secret does not exist yet: the image is unavailable.
	require.Equal(t, http.StatusServiceUnavailable, f.get("/vmlinuz").Code)

	secret := dockerConfigJSONSecret("netboot-pull", `{"auths":{"ghcr.io":{"username":"user","password":"pass"}}}`)
	secret.Namespace = "tenant-a"
	require.NoError(t, f.client.Create(t.Context(), secret))

	w := f.get("/vmlinuz")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "private kernel", w.Body.String())

	// Wrong credentials are rejected by the registry.
	secret.Data[".dockerconfigjson"] = []byte(`{"auths":{"ghcr.io":{"username":"user","password":"wrong"}}}`)
	require.NoError(t, f.client.Update(t.Context(), secret))
	require.Equal(t, http.StatusServiceUnavailable, f.get("/vmlinuz").Code)
}

func TestTFTPServerStreamsStaticFile(t *testing.T) {
	kernel := bigPayload(256 << 10)
	f := newFileServerFixture(t, map[string][]byte{"vmlinuz": kernel})
	server := &TFTPServer{FileResolver: f.server.FileResolver}

	var dst bytes.Buffer

	transfer := &sizedTFTPReaderFrom{tftpReaderFrom: tftpReaderFrom{readerFrom: readerFrom{Writer: &dst}, ip: fixtureIP}}
	require.NoError(t, server.readHandler("vmlinuz", transfer))
	require.Equal(t, kernel, dst.Bytes())
	require.Equal(t, int64(len(kernel)), transfer.size)

	// An absent file fails the transfer.
	dst.Reset()
	require.ErrorIs(t, server.readHandler("missing", transfer), imagepull.ErrNotFound)
	require.Empty(t, dst.Bytes())
}

type sizedTFTPReaderFrom struct {
	tftpReaderFrom
	size int64
}

func (s *sizedTFTPReaderFrom) SetSize(n int64) { s.size = n }

func TestMetadataMemo(t *testing.T) {
	var (
		mu  sync.Mutex
		now = time.Unix(1000, 0)
	)

	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()

		now = now.Add(d)
	}

	var (
		calls   atomic.Int32
		failing atomic.Bool
	)

	fetch := func(context.Context, ImageSource) (*ImageMetadata, error) {
		calls.Add(1)

		if failing.Load() {
			return nil, ErrImageUnavailable
		}

		return &ImageMetadata{DHCPBootImageName: fmt.Sprint("shim-", calls.Load())}, nil
	}

	memo := &MetadataMemo{TTL: time.Minute, ErrorTTL: 5 * time.Second, Now: clock}
	src := ImageSource{Ref: "ghcr.io/test/netboot:v1"}

	meta, err := memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	require.Equal(t, "shim-1", meta.DHCPBootImageName)

	// Reused within the TTL.
	advance(59 * time.Second)

	meta, err = memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	require.Equal(t, "shim-1", meta.DHCPBootImageName)
	require.EqualValues(t, 1, calls.Load())

	// Keys include architecture and pull secret.
	_, err = memo.get(t.Context(), ImageSource{Ref: src.Ref, Architecture: "arm64"}, fetch)
	require.NoError(t, err)
	_, err = memo.get(t.Context(), ImageSource{Ref: src.Ref, PullSecretRef: secretRef("a", "b")}, fetch)
	require.NoError(t, err)
	require.EqualValues(t, 3, calls.Load())

	// Refetched after the TTL; failures are memoized for ErrorTTL.
	advance(2 * time.Second)
	failing.Store(true)

	_, err = memo.get(t.Context(), src, fetch)
	require.ErrorIs(t, err, ErrImageUnavailable)
	require.EqualValues(t, 4, calls.Load())

	failing.Store(false)

	_, err = memo.get(t.Context(), src, fetch)
	require.ErrorIs(t, err, ErrImageUnavailable)
	require.EqualValues(t, 4, calls.Load())

	advance(5 * time.Second)

	meta, err = memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	require.Equal(t, "shim-5", meta.DHCPBootImageName)
}

func TestMetadataFlightKeyIsCollisionFree(t *testing.T) {
	a := metadataKey{ref: "a\x00b", architecture: "c"}
	b := metadataKey{ref: "a", architecture: "b\x00c"}
	require.NotEqual(t, a.flightKey(), b.flightKey())

	c := metadataKey{ref: "r", secretNamespace: "a", secretName: "b", hasSecret: true}
	d := metadataKey{ref: "r", secretNamespace: "a", secretName: "b"}
	require.NotEqual(t, c.flightKey(), d.flightKey())
	require.Equal(t, c.flightKey(), c.flightKey())
}

func TestMetadataMemoMissingMetadataUsesErrorTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	memo := &MetadataMemo{TTL: time.Minute, ErrorTTL: 5 * time.Second, Now: func() time.Time { return now }}

	var calls atomic.Int32

	fetch := func(context.Context, ImageSource) (*ImageMetadata, error) {
		calls.Add(1)
		return &ImageMetadata{missing: true}, nil
	}

	src := ImageSource{Ref: "r"}
	_, err := memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	_, err = memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())

	now = now.Add(6 * time.Second)
	_, err = memo.get(t.Context(), src, fetch)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load(), "missing metadata is refetched after ErrorTTL")
}

func TestFetchMetadataMissingIsMarked(t *testing.T) {
	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/nometa:v1", map[string][]byte{"vmlinuz": []byte("k")})
	images.Push(t, "ghcr.io/test/meta:v1", map[string][]byte{"metadata.yaml": []byte("dhcpBootImageName: x\n")})
	resolver := &FileResolver{Puller: images.Puller}

	meta, err := resolver.fetchMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/nometa:v1"})
	require.NoError(t, err)
	require.True(t, meta.missing)

	meta, err = resolver.fetchMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/meta:v1"})
	require.NoError(t, err)
	require.False(t, meta.missing)
}

func TestMetadataMemoDefaults(t *testing.T) {
	memo := &MetadataMemo{}
	require.Equal(t, DefaultMetadataTTL, memo.ttl(&ImageMetadata{}, nil))
	require.Equal(t, DefaultMetadataErrorTTL, memo.ttl(&ImageMetadata{missing: true}, nil))
	require.Equal(t, DefaultMetadataErrorTTL, memo.ttl(nil, errors.New("x")))

	memo.ErrorTTL = -1

	var calls atomic.Int32

	fetch := func(context.Context, ImageSource) (*ImageMetadata, error) {
		calls.Add(1)
		return nil, errors.New("boom")
	}

	for range 2 {
		_, err := memo.get(t.Context(), ImageSource{Ref: "r"}, fetch)
		require.Error(t, err)
	}

	require.EqualValues(t, 2, calls.Load(), "negative ErrorTTL disables error memoization")
}

func TestMetadataMemoDeduplicatesConcurrentLookups(t *testing.T) {
	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/netboot:v1", map[string][]byte{"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n")})
	images.Registry.ResetRequests()

	resolver := &FileResolver{Puller: images.Puller, Metadata: &MetadataMemo{}}
	src := ImageSource{Ref: "ghcr.io/test/netboot:v1"}

	var wg sync.WaitGroup

	errs := make(chan error, 32)

	for range 32 {
		wg.Go(func() {
			meta, err := resolver.ImageMetadata(t.Context(), src)
			if err == nil && meta.DHCPBootImageName != "shimx64.efi" {
				err = fmt.Errorf("unexpected metadata %+v", meta)
			}

			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, 1, images.Registry.Count(imagepulltest.KindManifest))

	// The memoized result serves later lookups without registry traffic.
	_, err := resolver.ImageMetadata(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, 1, images.Registry.Count(imagepulltest.KindManifest))
}

func TestMetadataMemoCanceledCallerDoesNotCancelSharedFetch(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	var calls atomic.Int32

	fetch := func(ctx context.Context, _ ImageSource) (*ImageMetadata, error) {
		calls.Add(1)
		close(started)
		<-release

		return &ImageMetadata{HTTPBootPath: "shim"}, ctx.Err()
	}

	memo := &MetadataMemo{}
	src := ImageSource{Ref: "r"}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		_, err := memo.get(ctx, src, fetch)
		done <- err
	}()

	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)

	require.Eventually(t, func() bool {
		meta, err := memo.get(t.Context(), src, fetch)
		return err == nil && meta.HTTPBootPath == "shim"
	}, 5*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
}

// corruptTemplateBlob replaces the served bytes of layer with a copy in
// which the first occurrence of marker has one byte flipped. The layer must
// be uncompressed so that the tar layout, and therefore the layer index,
// stays valid.
func corruptTemplateBlob(t *testing.T, reg *imagepulltest.Registry, repo, layer, marker string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, reg.URL()+"/v2/"+repo+"/blobs/"+layer, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	orig, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.NoError(t, err)

	blob := bytes.Clone(orig)
	i := bytes.Index(blob, []byte(marker))
	require.GreaterOrEqual(t, i, 0, "marker %q not found", marker)

	blob[i] ^= 0x20
	reg.SetBlob(layer, blob)
}

func TestTemplateRejectsChangedBytesAfterIndexing(t *testing.T) {
	const tmpl = "rendered for {{ .Machine.Name }} marker"

	f := newFileServerFixture(t, map[string][]byte{"vmlinuz": []byte("kernel")})
	_, dgst := f.images.Registry.Push(t, "test/netboot", "v1", imagepulltest.ImageSpec{Layers: []imagepulltest.Layer{{
		Files:       diskFiles(map[string][]byte{"grub.cfg.tmpl": []byte(tmpl)}),
		Compression: imagepulltest.Uncompressed,
	}}})
	layer := f.images.Registry.LayerDigests(dgst)[0]

	// The first request indexes the layer and serves the good template.
	w := f.get("/grub.cfg")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "rendered for pull-node marker", w.Body.String())

	// The registry now serves changed template bytes at the same offsets.
	corruptTemplateBlob(t, f.images.Registry, "test/netboot", layer, "marker")

	w = f.get("/grub.cfg")
	require.NotEqual(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "pull-node")
	require.NotContains(t, w.Body.String(), "mArker")

	// readTemplateFile itself surfaces the digest mismatch.
	file, isTmpl, err := f.server.openImageFile(t.Context(), ImageSource{Ref: "ghcr.io/test/netboot:v1"}, "grub.cfg")
	require.NoError(t, err)
	require.True(t, isTmpl)

	_, err = readTemplateFile(file)
	require.ErrorIs(t, err, imagepull.ErrDigestMismatch)
	require.ErrorIs(t, err, ErrImageUnavailable)
}

// mirrorPassthroughTransport sends requests for the mirror host unchanged
// and every other request to the in-memory registry.
type mirrorPassthroughTransport struct {
	mirror   string
	upstream *redirectTransport
}

func (rt *mirrorPassthroughTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == rt.mirror {
		return http.DefaultTransport.RoundTrip(req)
	}

	return rt.upstream.RoundTrip(req)
}

// A mirror that stalls before sending headers must not consume the whole
// metadata fetch budget: with the default mirror timeouts (minutes), a
// fetch budget shorter than those timeouts still resolves metadata from the
// upstream registry.
func TestMetadataMemoFallsBackWhenMirrorStalls(t *testing.T) {
	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/netboot:v1", map[string][]byte{"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n")})

	done := make(chan struct{})

	var stalls atomic.Int32

	mirror := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		stalls.Add(1)

		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))

	t.Cleanup(func() {
		close(done)
		mirror.Close()
	})

	mirrorURL, err := url.Parse(mirror.URL)
	require.NoError(t, err)

	puller := &imagepull.Puller{
		Mirror: mirror.URL,
		HTTPClient: &http.Client{Transport: &mirrorPassthroughTransport{
			mirror:   mirrorURL.Host,
			upstream: &redirectTransport{host: images.Registry.Host()},
		}},
	}

	// A scaled-down stand-in for DefaultMetadataFetchTimeout.
	resolver := &FileResolver{Puller: puller, Metadata: &MetadataMemo{FetchTimeout: 4 * time.Second}}

	// Resolve by digest so the manifest and blobs go through the mirror.
	tagged, err := (&imagepull.Puller{HTTPClient: images.Puller.HTTPClient}).Resolve(t.Context(), imagepull.Source{Ref: "ghcr.io/test/netboot:v1"})
	require.NoError(t, err)

	meta, err := resolver.ImageMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/netboot@" + tagged.Digest()})
	require.NoError(t, err)
	require.Equal(t, "shimx64.efi", meta.DHCPBootImageName)
	require.Positive(t, stalls.Load(), "mirror never stalled")
}
