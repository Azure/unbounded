// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

const (
	streamModeNormal int32 = iota
	streamModeFail
	streamModeTruncate
)

// streamLimit is the absolute layer offset at which streamModeTruncate cuts
// every blob response, so resumed Range requests cannot make progress.
const streamLimit = 32 << 10

type streamFixture struct {
	images   *testImages
	layer    string
	mode     atomic.Int32
	recorder *recordingStatusRecorder
	client   client.Client
	url      string
	shim     []byte
}

// limitWriter forwards at most n bytes and then aborts the response.
type limitWriter struct {
	http.ResponseWriter
	n int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.n {
		p = p[:w.n]
	}

	n, err := w.ResponseWriter.Write(p)
	w.n -= int64(n)

	if w.n <= 0 {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}

		panic(http.ErrAbortHandler)
	}

	return n, err
}

func rangeStart(h string) int64 {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0
	}

	start, _, _ := strings.Cut(spec, "-")
	n, _ := strconv.ParseInt(start, 10, 64) //nolint:errcheck // zero on error

	return n
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()

	f := &streamFixture{recorder: &recordingStatusRecorder{}}

	f.shim = append(bigPayload(64<<10), []byte("TAILMARK")...)

	f.images = newTestImages(t, imagepulltest.WithMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if f.layer == "" || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blobs/"+f.layer) {
				next.ServeHTTP(w, r)
				return
			}

			switch f.mode.Load() {
			case streamModeFail:
				http.Error(w, "unavailable", http.StatusInternalServerError)
			case streamModeTruncate:
				limit := streamLimit - rangeStart(r.Header.Get("Range"))
				if limit <= 0 {
					http.Error(w, "unavailable", http.StatusInternalServerError)
					return
				}

				next.ServeHTTP(&limitWriter{ResponseWriter: w, n: limit}, r)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}))

	parsed := parseTestRef(t, "ghcr.io/test/image:v1")
	_, dgst := f.images.Registry.Push(t, parsed.Repository, parsed.Reference, imagepulltest.ImageSpec{Layers: []imagepulltest.Layer{{
		Files: diskFiles(map[string][]byte{
			"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n"),
			"shimx64.efi":   f.shim,
			"vmlinuz":       []byte("kernel"),
		}),
		Compression: imagepulltest.Uncompressed,
	}}})
	f.layer = f.images.Registry.LayerDigests(dgst)[0]

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http"},
		Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
			Image:        "ghcr.io/test/image:v1",
			BootProtocol: v1alpha3.PXEBootProtocolHTTP,
			DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:10", IPv4: "10.0.1.60", SubnetMask: "255.255.255.0"}},
		}},
	}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "replace-node-http"},
		Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: node.Name},
		Status: v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress,
			Targets: []v1alpha3.MachineOperationTargetStatus{{
				MachineRef: node.Name,
				Phase:      v1alpha3.OperationPhaseInProgress,
				Stage:      v1alpha3.OperationStageWaitingRepave,
			}},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects([]client.Object{node, op}...).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()

	f.client = fc
	srv := &HTTPServer{
		FileResolver:   FileResolver{Puller: f.images.Puller, Reader: fc},
		StatusRecorder: f.recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	t.Cleanup(ts.Close)
	f.url = ts.URL

	// Build the layer index and metadata memo from the intact layer.
	resp, body, err := f.get(t, "/vmlinuz", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "kernel", string(body))

	return f
}

func (f *streamFixture) get(t *testing.T, path, rng string) (*http.Response, []byte, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+path, nil)
	require.NoError(t, err)
	req.Header.Set(testPeerHeader, "10.0.1.60")

	if rng != "" {
		req.Header.Set("Range", rng)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // test

	body, err := io.ReadAll(resp.Body)

	return resp, body, err
}

func TestHTTPServerStreamSucceedsAndRecords(t *testing.T) {
	f := newStreamFixture(t)

	resp, body, err := f.get(t, "/shimx64.efi", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, f.shim, body)
	// The handler records after the body is written, so wait for it.
	require.Eventually(t, func() bool {
		return len(f.recorder.bootLoaderEvents()) == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"node-http:shimx64.efi"}, f.recorder.bootLoaderEvents())

	resp, body, err = f.get(t, "/shimx64.efi", "bytes=-8")
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "TAILMARK", string(body))
}

func TestHTTPServerStreamInitialFailureReturns503(t *testing.T) {
	f := newStreamFixture(t)
	f.mode.Store(streamModeFail)

	for _, rng := range []string{"", "bytes=100-199"} {
		resp, _, err := f.get(t, "/shimx64.efi", rng)
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "range %q", rng)
		require.Equal(t, "5", resp.Header.Get("Retry-After"))
	}

	require.Empty(t, f.recorder.bootLoaderEvents())
}

func TestHTTPServerStreamMidTransferFailureAborts(t *testing.T) {
	f := newStreamFixture(t)
	f.mode.Store(streamModeTruncate)

	resp, body, err := f.get(t, "/shimx64.efi", "")
	require.Error(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Less(t, len(body), len(f.shim))
	require.Empty(t, f.recorder.bootLoaderEvents())
}

func TestHTTPServerStreamDigestMismatchWithholdsTail(t *testing.T) {
	f := newStreamFixture(t)
	f.corruptTail(t)

	resp, body, err := f.get(t, "/shimx64.efi", "")
	require.Error(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Less(t, len(body), len(f.shim))
	require.NotContains(t, string(body), "tAILMARK")
	require.Equal(t, f.shim[:len(body)], body)
	require.Empty(t, f.recorder.bootLoaderEvents())
}

func (f *streamFixture) corruptTail(t *testing.T) {
	t.Helper()

	resp, err := http.Get(f.images.Registry.URL() + "/v2/test/image/blobs/" + f.layer) //nolint:noctx // test
	require.NoError(t, err)

	blob, err := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck,gosec // test
	require.NoError(t, err)

	i := bytes.Index(blob, []byte("TAILMARK"))
	require.GreaterOrEqual(t, i, 0)

	blob = bytes.Clone(blob)
	blob[i] ^= 0x20
	f.images.Registry.SetBlob(f.layer, blob)
}

func TestTFTPServerStreamFailureNotRecorded(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *streamFixture){
		"truncated":       func(_ *testing.T, f *streamFixture) { f.mode.Store(streamModeTruncate) },
		"digest mismatch": func(t *testing.T, f *streamFixture) { f.corruptTail(t) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newStreamFixture(t)
			recorder := &recordingTFTPStatusRecorder{}
			srv := &TFTPServer{
				FileResolver:   FileResolver{Puller: f.images.Puller, Reader: f.client},
				StatusRecorder: recorder,
			}

			var out bytes.Buffer

			rf := &tftpReaderFrom{readerFrom: readerFrom{&out}, ip: "10.0.1.60"}
			require.NoError(t, srv.readHandler("shimx64.efi", rf))
			require.Equal(t, f.shim, out.Bytes())
			require.Equal(t, []string{"node-http:shimx64.efi"}, recorder.calls)

			setup(t, f)
			out.Reset()

			recorder.calls = nil

			require.Error(t, srv.readHandler("shimx64.efi", rf))
			require.Less(t, out.Len(), len(f.shim))
			require.NotContains(t, out.String(), "tAILMARK")
			require.Empty(t, recorder.calls)
		})
	}
}

func TestPrimeOffset(t *testing.T) {
	for rng, want := range map[string]int64{
		"":              0,
		"bytes=10-20":   10,
		"bytes=10-":     10,
		"bytes=99-99":   99,
		"bytes=-30":     70,
		"bytes=-300":    0,
		"bytes=-0":      0,
		"bytes=-abc":    0,
		"bytes=0-1,5-6": 0,
		"bytes=100-":    0,
		"bytes=abc-":    0,
		"bytes=10":      0,
		"items=10-20":   0,
	} {
		require.Equal(t, want, primeOffset(rng, 100), "range %q", rng)
	}
}
