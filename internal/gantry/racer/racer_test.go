// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type testUpstream struct {
	head  func(context.Context, ifaces.OriginRef) (int64, string, error)
	pull  func(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error)
	heads atomic.Int64
	pulls atomic.Int64
}

func (u *testUpstream) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	u.heads.Add(1)
	return u.head(ctx, ref)
}

func (u *testUpstream) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	u.pulls.Add(1)
	return u.pull(ctx, ref)
}

func testRef() ifaces.OriginRef {
	return ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: digestOf([]byte("fixture"))}
}

func digestOf(data []byte) digest.Digest {
	return digest.MustParse(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

func testConfig() *config.Config {
	return &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example", NSAlias: "alias.example"}}}
}

func requireKind(t *testing.T, err error, want racersdk.ErrorKind) {
	t.Helper()

	var typed *racersdk.Error
	if !errors.As(err, &typed) || typed.Kind() != want {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func fakeClient(t *testing.T, origin racersdk.Origin) *racersdk.Client {
	t.Helper()

	client, cleanup, err := racersdk.NewFakeClient(origin)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	return client
}

func requestFor(t *testing.T, ref ifaces.OriginRef, authorization string) racersdk.Request {
	t.Helper()

	req, err := Request(ref, authorization)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func TestRequest(t *testing.T) {
	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindConfig, ifaces.KindManifest} {
		t.Run(kind.String(), func(t *testing.T) {
			ref := testRef()
			ref.Kind = kind

			req := requestFor(t, ref, "Bearer private-token")
			if req.Key.String() != ref.Digest.Hex() {
				t.Fatalf("key = %s", req.Key)
			}

			if strings.Contains(req.Context.Metadata().ForOrigin(), "private-token") || strings.Contains(fmt.Sprintf("%#v", req), "private-token") {
				t.Fatal("authorization leaked into metadata or diagnostics")
			}

			if req.Context.Authorization().ForOrigin() != "Bearer private-token" {
				t.Fatal("missing separate authorization")
			}

			decoded, err := decodeReference(req.Key, req.Context.Metadata())
			if err != nil || decoded != ref {
				t.Fatalf("round trip = %+v, %v", decoded, err)
			}
		})
	}
}

func TestRequestRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ifaces.OriginRef)
	}{
		{"tag", func(r *ifaces.OriginRef) { r.Digest, _ = digest.Parse("latest") }},
		{"algorithm", func(r *ifaces.OriginRef) { r.Digest, _ = digest.Parse("sha512:" + strings.Repeat("a", 128)) }},
		{"uppercase", func(r *ifaces.OriginRef) { r.Digest, _ = digest.Parse("sha256:" + strings.Repeat("A", 64)) }},
		{"offset", func(r *ifaces.OriginRef) { r.Offset = 1 }},
		{"negative offset", func(r *ifaces.OriginRef) { r.Offset = -1 }},
		{"kind", func(r *ifaces.OriginRef) { r.Kind = 99 }},
		{"repository", func(r *ifaces.OriginRef) { r.Repository = "../secret" }},
		{"registry credentials", func(r *ifaces.OriginRef) { r.Registry = "user:secret@registry.example" }},
		{"registry URL", func(r *ifaces.OriginRef) { r.Registry = "https://registry.example" }},
		{"registry query", func(r *ifaces.OriginRef) { r.Registry += "?token=secret" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := testRef()
			tc.change(&ref)
			_, err := Request(ref, "")
			requireKind(t, err, racersdk.ErrorInvalidArgument)
		})
	}

	for _, auth := range []string{"Digest private", "Basic invalid", "Bearer", "Bearer a\r\nb", "Bearer " + strings.Repeat("a", 8192)} {
		if _, err := Request(testRef(), auth); err == nil {
			t.Fatal("accepted invalid authorization")
		}
	}
}

func TestFakeClientPagesAndCredentials(t *testing.T) {
	for _, size := range []int{0, 17, int(racersdk.PageSize), int(2*racersdk.PageSize) + 37} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{0x5a}, size)
			ref := testRef()
			ref.Registry = "alias.example"
			ref.Digest = digestOf(data)

			const auth = "Basic dXNlcjpzZWNyZXQ="

			var (
				closed         atomic.Int64
				expectedOffset atomic.Int64
			)

			upstream := &testUpstream{
				head: func(ctx context.Context, got ifaces.OriginRef) (int64, string, error) {
					if got != ref || registryauth.Authorization(ctx) != auth {
						return 0, "", errors.New("incorrect HEAD reference or credential")
					}

					return int64(size), "application/octet-stream", nil
				},
				pull: func(ctx context.Context, got ifaces.OriginRef) (io.ReadCloser, int64, error) {
					if got.Offset != expectedOffset.Load() || got.Digest != ref.Digest || registryauth.Authorization(ctx) != auth {
						return nil, 0, errors.New("incorrect page offset or credential")
					}

					expectedOffset.Add(int64(racersdk.PageSize))

					return &trackedBody{ReadCloser: io.NopCloser(bytes.NewReader(data[got.Offset:])), closed: &closed}, int64(size), nil
				},
			}
			client := fakeClient(t, Origin(testConfig(), upstream))
			before := time.Now()

			value, err := client.Get(context.Background(), requestFor(t, ref, auth))
			if err != nil {
				t.Fatal(err)
			}
			defer value.Close()

			metadata := value.Metadata()
			if metadata.Validate() != nil || metadata.Size != racersdk.ByteLength(size) || metadata.ETag.String() != `"`+ref.Digest.String()+`"` {
				t.Fatalf("invalid metadata: %+v", metadata)
			}

			if metadata.ExpiresAt.Before(before.Add(metadataTTL-time.Millisecond)) || metadata.ExpiresAt.After(time.Now().Add(metadataTTL)) {
				t.Fatalf("expiry outside bounded TTL: %v", metadata.ExpiresAt)
			}

			got, err := io.ReadAll(value)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("read size = %d, want %d, error = %v", len(got), size, err)
			}

			pages := (int64(size) + int64(racersdk.PageSize) - 1) / int64(racersdk.PageSize)
			if upstream.pulls.Load() != pages || upstream.heads.Load() != max(1, pages) {
				t.Fatalf("heads/pulls = %d/%d, pages = %d", upstream.heads.Load(), upstream.pulls.Load(), pages)
			}

			deadline := time.After(5 * time.Second)

			for closed.Load() != pages {
				select {
				case <-deadline:
					t.Fatalf("closed %d of %d upstream bodies", closed.Load(), pages)
				default:
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

type trackedBody struct {
	io.ReadCloser
	closed *atomic.Int64
}

func (b *trackedBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func TestOriginRejectsUntrustedMetadata(t *testing.T) {
	for _, metadata := range []string{
		`null`, `{}`, `{"version":2,"registry":"registry.example","repository":"library/image","kind":"blob"}`,
		`{"version":1,"registry":"evil.example","repository":"library/image","kind":"blob"}`,
		`{"version":1,"registry":"registry.example","repository":"../secrets","kind":"blob"}`,
		`{"version":1,"registry":"registry.example","repository":"library/image","kind":"unknown"}`,
		`{"version":1,"registry":"registry.example","repository":"library/image","kind":"blob","authorization":"Bearer secret"}`,
		`{"version":1,"registry":"registry.example","repository":"library/image","kind":"blob"} {}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			upstream := &testUpstream{}
			client := fakeClient(t, Origin(testConfig(), upstream))

			m, err := racersdk.ParseAdapterMetadata(metadata)
			if err != nil {
				t.Fatal(err)
			}

			fetchContext, err := racersdk.NewFetchContext(m, racersdk.Authorization{})
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Get(context.Background(), racersdk.Request{Context: fetchContext})
			requireKind(t, err, racersdk.ErrorInvalidArgument)

			if upstream.heads.Load()+upstream.pulls.Load() != 0 {
				t.Fatal("untrusted metadata reached upstream")
			}
		})
	}
}

func TestOriginRejectsUntrustedAuthorization(t *testing.T) {
	upstream := &testUpstream{}
	client := fakeClient(t, Origin(testConfig(), upstream))
	req := requestFor(t, testRef(), "")

	auth, err := racersdk.ParseAuthorization("Digest unsupported")
	if err != nil {
		t.Fatal(err)
	}

	req.Context, err = racersdk.NewFetchContext(req.Context.Metadata(), auth)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Get(context.Background(), req)
	requireKind(t, err, racersdk.ErrorInvalidArgument)

	if upstream.heads.Load()+upstream.pulls.Load() != 0 {
		t.Fatal("malformed authorization reached upstream")
	}
}

func TestOriginHeadPinAndRanges(t *testing.T) {
	ref := testRef()

	tag, err := racersdk.ParseETag(`"` + ref.Digest.String() + `"`)
	if err != nil {
		t.Fatal(err)
	}

	wrongPin, err := racersdk.ParseETag(`"wrong"`)
	if err != nil {
		t.Fatal(err)
	}

	page, err := racersdk.ClosedRange(0, racersdk.ByteOffset(racersdk.PageSize-1))
	if err != nil {
		t.Fatal(err)
	}

	for _, operation := range []racersdk.Operation{racersdk.OperationHead, racersdk.OperationPinned} {
		upstream := &testUpstream{}
		_, body, err := open(context.Background(), upstream, ref, operation, wrongPin, page)
		requireKind(t, err, racersdk.ErrorVersionUnavailable)

		if body != nil || upstream.heads.Load()+upstream.pulls.Load() != 0 {
			t.Fatal("pin mismatch performed upstream I/O")
		}
	}

	upstream := &testUpstream{head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return 0, "", nil }}
	for _, operation := range []racersdk.Operation{racersdk.OperationHead, racersdk.OperationBootstrap} {
		metadata, body, err := open(context.Background(), upstream, ref, operation, racersdk.ETag{}, page)
		if err != nil || metadata.Size != 0 || metadata.Validate() != nil || body != nil {
			t.Fatalf("empty operation %v: %+v, %v", operation, metadata, err)
		}
	}

	metadata, body, err := open(context.Background(), upstream, ref, racersdk.OperationPinned, tag, page)
	requireKind(t, err, racersdk.ErrorUnsatisfiableRange)

	if body != nil || metadata.Size != 0 || metadata.Validate() != nil || upstream.pulls.Load() != 0 {
		t.Fatal("empty pinned read must return valid 416 metadata without Pull")
	}

	for _, tc := range []struct {
		first, last racersdk.ByteOffset
		kind        racersdk.ErrorKind
	}{
		{1, 10, racersdk.ErrorInvalidArgument},
		{0, 10, racersdk.ErrorInvalidArgument},
		{0, racersdk.ByteOffset(racersdk.PageSize), racersdk.ErrorInvalidArgument},
		{racersdk.ByteOffset(2 * racersdk.PageSize), racersdk.ByteOffset(3*racersdk.PageSize - 1), racersdk.ErrorUnsatisfiableRange},
	} {
		page, err := racersdk.ClosedRange(tc.first, tc.last)
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = resolvePage(page, racersdk.PageSize+3)
		requireKind(t, err, tc.kind)
	}

	for _, last := range []racersdk.ByteOffset{racersdk.ByteOffset(racersdk.PageSize + 2), racersdk.ByteOffset(2*racersdk.PageSize - 1)} {
		page, err := racersdk.ClosedRange(racersdk.ByteOffset(racersdk.PageSize), last)
		if err != nil {
			t.Fatal(err)
		}

		first, end, err := resolvePage(page, racersdk.PageSize+3)
		if err != nil || first != racersdk.ByteOffset(racersdk.PageSize) || end != racersdk.ByteOffset(racersdk.PageSize+2) {
			t.Fatalf("final page = %d-%d, %v", first, end, err)
		}
	}
}

func TestOriginErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind racersdk.ErrorKind
	}{
		{"auth", &ifaces.OriginError{Class: ifaces.FailureAuth}, racersdk.ErrorUnauthorized},
		{"not found", &ifaces.OriginError{Class: ifaces.FailureNotFound}, racersdk.ErrorNotFound},
		{"rate limit", &ifaces.OriginError{Class: ifaces.FailureRateLimited}, racersdk.ErrorUnavailable},
		{"transient", &ifaces.OriginError{Class: ifaces.FailureTransient}, racersdk.ErrorUnavailable},
		{"unknown", errors.New("private-token"), racersdk.ErrorBadGateway},
		{"canceled", context.Canceled, racersdk.ErrorUnavailable},
		{"deadline", context.DeadlineExceeded, racersdk.ErrorUnavailable},
	} {
		for _, stage := range []string{"head", "pull"} {
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				upstream := &testUpstream{
					head: func(context.Context, ifaces.OriginRef) (int64, string, error) {
						if stage == "head" {
							return 0, "", tc.err
						}

						return 1, "", nil
					},
					pull: func(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) { return nil, 0, tc.err },
				}
				client := fakeClient(t, Origin(testConfig(), upstream))
				_, err := client.Get(context.Background(), requestFor(t, testRef(), ""))
				requireKind(t, err, tc.kind)

				if strings.Contains(err.Error(), "private-token") {
					t.Fatal("error leaked cause")
				}

				if upstream.heads.Load() != 1 || upstream.pulls.Load() > 1 {
					t.Fatal("failure retried upstream")
				}
			})
		}
	}
}

func TestOriginInvalidSizesAndShortBody(t *testing.T) {
	for _, tc := range []struct {
		name               string
		headSize, pullSize int64
		data               string
		nilBody            bool
		late               bool
	}{
		{name: "unknown HEAD size", headSize: -1},
		{name: "unknown Pull size", headSize: 1, pullSize: -1, data: "a"},
		{name: "changed size", headSize: 1, pullSize: 2, data: "ab"},
		{name: "missing body", headSize: 1, pullSize: 1, nilBody: true},
		{name: "short body", headSize: 2, pullSize: 2, data: "a", late: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &testUpstream{
				head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return tc.headSize, "", nil },
				pull: func(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
					if tc.nilBody {
						return nil, tc.pullSize, nil
					}

					return io.NopCloser(strings.NewReader(tc.data)), tc.pullSize, nil
				},
			}
			client := fakeClient(t, Origin(testConfig(), upstream))

			value, err := client.Get(context.Background(), requestFor(t, testRef(), ""))
			if !tc.late {
				requireKind(t, err, racersdk.ErrorBadGateway)
				return
			}

			if err == nil {
				defer value.Close()

				_, err = io.ReadAll(value)
			}

			if err == nil {
				t.Fatal("short upstream body accepted")
			}
		})
	}
}

func TestPageBodyCloseInterruptsRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	body := &pageBody{Reader: io.LimitReader(reader, 10), upstream: reader}
	readDone := make(chan error, 1)

	go func() {
		_, err := body.Read(make([]byte, 1))
		readDone <- err
	}()

	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not interrupt upstream Read")
	}
}

func TestFakeClientContinuationFailureIsTerminal(t *testing.T) {
	size := int64(racersdk.PageSize) + 1
	upstream := &testUpstream{
		head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return size, "", nil },
		pull: func(_ context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
			if ref.Offset != 0 {
				return nil, 0, &ifaces.OriginError{Class: ifaces.FailureNotFound}
			}

			return io.NopCloser(io.LimitReader(zeroReader{}, size)), size, nil
		},
	}
	client := fakeClient(t, Origin(testConfig(), upstream))

	value, err := client.Get(context.Background(), requestFor(t, testRef(), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	n, err := io.Copy(io.Discard, value)
	if err == nil || n >= size {
		t.Fatalf("missing continuation succeeded: bytes=%d, error=%v", n, err)
	}

	if upstream.heads.Load() != 2 || upstream.pulls.Load() != 2 {
		t.Fatalf("unexpected retry or fallback: heads=%d, pulls=%d", upstream.heads.Load(), upstream.pulls.Load())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestFakeClientCleanupInterruptsUpstream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	closed := make(chan struct{})
	upstream := &testUpstream{
		head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return 10, "", nil },
		pull: func(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
			return &signalingBody{ReadCloser: reader, closed: closed}, 10, nil
		},
	}

	client, cleanup, err := racersdk.NewFakeClient(Origin(testConfig(), upstream))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	value, err := client.Get(context.Background(), requestFor(t, testRef(), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	cleanup()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("SDK cleanup did not close blocked upstream body")
	}
}

type signalingBody struct {
	io.ReadCloser
	closed chan struct{}
}

func (b *signalingBody) Close() error {
	err := b.ReadCloser.Close()
	close(b.closed)

	return err
}

func TestOriginNonemptyHeadAndCanceledContext(t *testing.T) {
	upstream := &testUpstream{
		head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return 123, "", nil },
	}

	metadata, body, err := open(context.Background(), upstream, testRef(), racersdk.OperationHead, racersdk.ETag{}, racersdk.Range{})
	if err != nil || metadata.Size != 123 || metadata.Validate() != nil || body != nil || upstream.pulls.Load() != 0 {
		t.Fatalf("HEAD result: %+v, body=%v, error=%v", metadata, body, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = open(ctx, upstream, testRef(), racersdk.OperationHead, racersdk.ETag{}, racersdk.Range{})
	requireKind(t, err, racersdk.ErrorCanceled)

	if !errors.Is(err, context.Canceled) || upstream.heads.Load() != 1 {
		t.Fatal("canceled context was lost or reached upstream")
	}
}

func TestRegistryOriginManifestContinuation(t *testing.T) {
	data := bytes.Repeat([]byte("m"), int(racersdk.PageSize)+23)

	var continuations atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))

		if r.Method == http.MethodHead {
			return
		}

		offset := 0

		if r.Header.Get("Range") != "" {
			if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", racersdk.PageSize) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			offset = int(racersdk.PageSize)

			continuations.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}

		_, _ = w.Write(data[offset:])
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.UpstreamRegistries[0].Endpoint = server.URL

	upstream, err := registryorigin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	client := fakeClient(t, Origin(cfg, upstream))
	ref := testRef()
	ref.Digest = digestOf(data)

	value, err := client.Get(context.Background(), requestFor(t, ref, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	got, err := io.ReadAll(value)
	if err != nil || !bytes.Equal(got, data) || continuations.Load() != 1 {
		t.Fatalf("manifest continuation: bytes=%d, ranges=%d, error=%v", len(got), continuations.Load(), err)
	}
}
