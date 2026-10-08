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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type testUpstream struct {
	head  func(context.Context, ifaces.OriginRef) (int64, string, error)
	pull  func(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error)
	heads atomic.Int64
	pulls atomic.Int64
}

// pageBody bounds fixture bytes while preserving cancellation through Close.
type pageBody struct {
	io.Reader
	upstream io.ReadCloser
}

func (b *pageBody) Close() error { return b.upstream.Close() }

func (u *testUpstream) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	u.heads.Add(1)
	return u.head(ctx, ref)
}

func (u *testUpstream) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	u.pulls.Add(1)
	// These callbacks describe the fixture metadata and bytes. No Head method
	// is called: bounded origin responses supply both in the same operation.
	metadataRef := ref
	metadataRef.Offset = 0

	size, contentType, err := u.head(ctx, metadataRef)
	if err != nil || size < 0 {
		return nil, size, contentType, err
	}

	if size == 0 && u.pull == nil {
		return io.NopCloser(strings.NewReader("")), 0, contentType, nil
	}

	body, pulledSize, err := u.pull(ctx, ref)
	if err != nil || body == nil {
		return body, pulledSize, contentType, err
	}

	if size != pulledSize {
		_ = body.Close()
		return nil, -1, contentType, nil
	}

	return &pageBody{Reader: io.LimitReader(body, length), upstream: body}, size, contentType, nil
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

func racerTestCache(t *testing.T) string {
	t.Helper()
	return CacheName
}

func TestGantryOriginEnablesOwnedRecoveryAfterDirectoryPreparation(t *testing.T) {
	root := t.TempDir()
	want := errors.New("serve result")
	called := false

	err := serveOriginAt(t.Context(), racersdk.OriginConfig{}, nil, root,
		func(_ context.Context, config racersdk.OriginConfig, _ racersdk.Origin) error {
			called = true

			if !config.RecoverStaleSocket {
				t.Fatal("Gantry did not opt into crash recovery")
			}

			if info, err := os.Stat(filepath.Join(root, "gantry", "origin")); err != nil || !info.IsDir() {
				t.Fatalf("directory not ready: %v", err)
			}

			return want
		})
	if !called || !errors.Is(err, want) {
		t.Fatalf("serve result: called=%v err=%v", called, err)
	}
}

func TestPrepareRacerOriginDirectory(t *testing.T) {
	for _, clientExists := range []bool{false, true} {
		name := "gantry-first"
		if clientExists {
			name = "racer-first"
		}

		t.Run(name, func(t *testing.T) {
			root := t.TempDir()

			client := filepath.Join(root, "gantry", "client")
			if clientExists {
				if err := os.MkdirAll(client, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(client, "socket"), []byte("preserve client"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// Concurrent startup may race with another creator of the volume path.
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					if err := prepareRacerOriginDirectory(root); err != nil {
						t.Error(err)
					}
				})
			}

			workers.Wait()

			origin := filepath.Join(root, "gantry", "origin")

			info, err := os.Lstat(origin)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o700 != 0o700 || info.Mode().Perm()&0o022 != 0 {
				t.Fatalf("origin directory: %v, %v", info, err)
			}

			if clientExists {
				data, err := os.ReadFile(filepath.Join(client, "socket"))
				if err != nil || string(data) != "preserve client" {
					t.Fatalf("client changed: %q, %v", data, err)
				}
			} else if _, err := os.Lstat(client); !os.IsNotExist(err) {
				t.Fatalf("created client endpoint: %v", err)
			}

			if err := os.Chmod(origin, 0o750); err != nil {
				t.Fatal(err)
			}

			socket := filepath.Join(origin, "socket")
			if err := os.WriteFile(socket, []byte("preserve origin"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := prepareRacerOriginDirectory(root); err != nil {
				t.Fatal(err)
			}

			info, err = os.Lstat(origin)
			if err != nil || info.Mode().Perm() != 0o750 {
				t.Fatalf("existing directory mode changed: %v, %v", info, err)
			}

			data, err := os.ReadFile(socket)
			if err != nil || string(data) != "preserve origin" {
				t.Fatalf("origin changed: %q, %v", data, err)
			}
		})
	}
}

func TestPrepareRacerOriginDirectoryRejectsUnsafePaths(t *testing.T) {
	for _, component := range []string{"mount", "gantry", "origin"} {
		for _, kind := range []string{"file", "symlink", "dangling-symlink"} {
			t.Run(component+"/"+kind, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "mount")

				path := root
				if component != "mount" {
					path = filepath.Join(path, "gantry")
				}

				if component == "origin" {
					path = filepath.Join(path, "origin")
				}

				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}

				target := t.TempDir()

				var err error

				switch kind {
				case "file":
					err = os.WriteFile(path, []byte("preserve"), 0o600)
				case "symlink":
					err = os.Symlink(target, path)
				case "dangling-symlink":
					err = os.Symlink(filepath.Join(target, "missing"), path)
				}

				if err != nil {
					t.Fatal(err)
				}

				if err := prepareRacerOriginDirectory(root); err == nil {
					t.Fatal("accepted unsafe directory")
				}

				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified symlink target: %v, %v", entries, err)
				}
			})
		}
	}

	t.Run("missing-mount", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		if err := prepareRacerOriginDirectory(root); !os.IsNotExist(err) {
			t.Fatalf("expected missing mount error, got %v", err)
		}

		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatalf("created missing mount: %v", err)
		}
	})
}

func TestRacerSDKConfigWiring(t *testing.T) {
	c := config.NewDefault()
	c.RacerMaxConnections = 7
	c.RacerOriginConcurrentRequests = 3
	cache := racerTestCache(t)

	client := ClientConfig(c, cache)
	if client.Cache != cache || client.MaxConnections != 7 {
		t.Fatalf("client config=%+v", client)
	}

	origin := OriginConfig(c, cache)
	if origin.Cache != cache || origin.MaxConcurrentRequests != 3 {
		t.Fatalf("origin config=%+v", origin)
	}
}

func TestRacerSDKDefaults(t *testing.T) {
	for _, test := range []struct {
		name                  string
		config                *config.Config
		connections, requests int
	}{
		{name: "defaults", config: config.NewDefault(), connections: 64, requests: 64},
		{name: "zero selects SDK defaults", config: &config.Config{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := racerTestCache(t)
			client := ClientConfig(test.config, cache)

			origin := OriginConfig(test.config, cache)
			if client.MaxConnections != test.connections || origin.MaxConcurrentRequests != test.requests {
				t.Fatalf("client=%+v origin=%+v", client, origin)
			}

			sdk, err := racersdk.NewClient(client)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := sdk.Close(); err != nil {
					t.Error(err)
				}
			})
		})
	}
}

func requireError(t *testing.T, err, want error) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func fakeClient(t *testing.T, origin racersdk.Origin) *racersdk.Client {
	t.Helper()

	return racersdktest.NewClient(t, origin)
}

func requestFor(t *testing.T, ref ifaces.OriginRef, authorization string) racersdk.Request {
	t.Helper()

	req, err := Request(ref, authorization)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func adapterGet(t *testing.T, client *racersdk.Client, ref ifaces.OriginRef, authorization string) *racersdk.Object {
	t.Helper()

	value, err := client.Get(t.Context(), requestFor(t, ref, authorization))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = value.Close() })

	return value
}

func adapterRegistryClient(t *testing.T, endpoint string) *racersdk.Client {
	t.Helper()

	cfg := testConfig()
	cfg.UpstreamRegistries[0].Endpoint = endpoint

	upstream, err := registryorigin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return fakeClient(t, Origin(cfg, upstream))
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

			if strings.Contains(req.Metadata, "private-token") || strings.Contains(fmt.Sprintf("%#v", req), "private-token") {
				t.Fatal("authorization leaked into metadata or diagnostics")
			}

			if req.Authorization != "Bearer private-token" {
				t.Fatal("missing separate authorization")
			}

			decoded, err := decodeReference(req.Key, req.Metadata)
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
			requireError(t, err, racersdk.ErrInvalidRequest)
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
				closed atomic.Int64
				seen   sync.Map
			)

			upstream := &testUpstream{
				head: func(ctx context.Context, got ifaces.OriginRef) (int64, string, error) {
					if got != ref || registryauth.Authorization(ctx) != auth {
						return 0, "", errors.New("incorrect HEAD reference or credential")
					}

					return int64(size), "application/octet-stream", nil
				},
				pull: func(ctx context.Context, got ifaces.OriginRef) (io.ReadCloser, int64, error) {
					if got.Offset < 0 || (got.Offset >= int64(size) && (size != 0 || got.Offset != 0)) || got.Offset%int64(racersdk.PageSize) != 0 || got.Digest != ref.Digest || registryauth.Authorization(ctx) != auth {
						return nil, 0, errors.New("incorrect page offset or credential")
					}

					if _, duplicate := seen.LoadOrStore(got.Offset, true); duplicate {
						return nil, 0, errors.New("duplicate page offset")
					}

					return &trackedBody{ReadCloser: io.NopCloser(bytes.NewReader(data[got.Offset:])), closed: &closed}, int64(size), nil
				},
			}
			client := fakeClient(t, Origin(testConfig(), upstream))
			before := time.Now()

			value := adapterGet(t, client, ref, auth)

			metadata := value.Metadata()
			if !validRacerMetadata(metadata, ref.Digest) || metadata.Size != int64(size) || metadata.ContentType != "application/octet-stream" {
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
			if upstream.pulls.Load() != max(1, pages) || upstream.heads.Load() != 0 {
				t.Fatalf("heads/pulls = %d/%d, pages = %d", upstream.heads.Load(), upstream.pulls.Load(), pages)
			}

			deadline := time.After(5 * time.Second)

			for closed.Load() != max(1, pages) {
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

			_, err := client.Get(context.Background(), racersdk.Request{Metadata: metadata})
			requireError(t, err, racersdk.ErrInvalidRequest)

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

	req.Authorization = "Digest unsupported"

	_, err := client.Get(context.Background(), req)
	requireError(t, err, racersdk.ErrInvalidRequest)

	if upstream.heads.Load()+upstream.pulls.Load() != 0 {
		t.Fatal("malformed authorization reached upstream")
	}
}

func TestOriginHeadPinAndRanges(t *testing.T) {
	ref := testRef()
	tag := `"` + ref.Digest.String() + `"`

	for _, head := range []bool{true, false} {
		upstream := &testUpstream{}
		_, body, err := open(t.Context(), upstream, ref, racersdk.OriginRequest{Head: head, ETag: `"wrong"`, Length: racersdk.PageSize})
		requireError(t, err, racersdk.ErrVersionMismatch)

		if body != nil || upstream.heads.Load()+upstream.pulls.Load() != 0 {
			t.Fatal("pin mismatch performed upstream I/O")
		}
	}

	upstream := &testUpstream{head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return 0, "", nil }}
	for _, request := range []racersdk.OriginRequest{{Head: true}, {Length: racersdk.PageSize}, {ETag: tag, Length: racersdk.PageSize}} {
		metadata, body, err := open(t.Context(), upstream, ref, request)
		if err != nil || metadata.Size != 0 || !validRacerMetadata(metadata, ref.Digest) || body != nil {
			t.Fatalf("empty request %v: %+v, %v", request, metadata, err)
		}
	}
}

func TestOriginEmptyRanges(t *testing.T) {
	for _, offset := range []int64{racersdk.PageSize, 2 * racersdk.PageSize} {
		for _, nilBody := range []bool{true, false} {
			t.Run(fmt.Sprintf("offset=%d/nil=%v", offset, nilBody), func(t *testing.T) {
				var closed atomic.Int64

				fixture := boundedFixture{testUpstream: &testUpstream{}, read: func(_ context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
					if ref.Offset != offset || length != racersdk.PageSize {
						t.Errorf("range changed: offset=%d length=%d", ref.Offset, length)
					}

					var body io.ReadCloser
					if !nilBody {
						body = &trackedBody{ReadCloser: io.NopCloser(strings.NewReader("")), closed: &closed}
					}

					return body, racersdk.PageSize, "application/octet-stream", nil
				}}

				metadata, body, err := Origin(testConfig(), fixture)(t.Context(), racersdk.OriginRequest{
					Request: requestFor(t, testRef(), ""), Offset: offset, Length: racersdk.PageSize,
				})
				if err != nil || body != nil || metadata.Size != racersdk.PageSize || !validRacerMetadata(metadata, testRef().Digest) {
					t.Fatalf("empty range: metadata=%+v body=%v err=%v", metadata, body, err)
				}

				wantClosed := int64(1)
				if nilBody {
					wantClosed = 0
				}

				if closed.Load() != wantClosed {
					t.Fatalf("body closes=%d want=%d", closed.Load(), wantClosed)
				}
			})
		}
	}
}

func TestRacerErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{racersdk.ErrNotFound, http.StatusNotFound},
		{racersdk.ErrUnauthorized, http.StatusUnauthorized},
		{racersdk.ErrForbidden, http.StatusForbidden},
		{racersdk.ErrUnavailable, http.StatusServiceUnavailable},
		{context.Canceled, http.StatusServiceUnavailable},
		{context.DeadlineExceeded, http.StatusServiceUnavailable},
		{net.ErrClosed, http.StatusServiceUnavailable},
		{racersdk.ErrInvalidRequest, http.StatusBadGateway},
		{racersdk.ErrVersionMismatch, http.StatusBadGateway},
		{racersdk.ErrRangeNotSatisfiable, http.StatusBadGateway},
		{io.ErrUnexpectedEOF, http.StatusBadGateway},
		{errors.New("unknown failure"), http.StatusBadGateway},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("wrapped: %w", tc.err)
			w := httptest.NewRecorder()
			writeRacerError(w, wrapped)

			if w.Code != tc.status || w.Body.String() != "Racer request failed\n" {
				t.Fatalf("response=%d %q", w.Code, w.Body.String())
			}

			requireError(t, classifyError(wrapped), tc.err)
		})
	}
}

func TestOriginErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind error
	}{
		{"auth", &ifaces.OriginError{Class: ifaces.FailureAuth}, racersdk.ErrUnauthorized},
		{"forbidden", &ifaces.OriginError{Class: ifaces.FailureAuth, StatusCode: 403}, racersdk.ErrForbidden},
		{"not found", &ifaces.OriginError{Class: ifaces.FailureNotFound}, racersdk.ErrNotFound},
		{"rate limit", &ifaces.OriginError{Class: ifaces.FailureRateLimited}, racersdk.ErrUnavailable},
		{"transient", &ifaces.OriginError{Class: ifaces.FailureTransient}, racersdk.ErrUnavailable},
		{"unknown", errors.New("private-token"), nil},
		{"canceled", context.Canceled, racersdk.ErrUnavailable},
		{"deadline", context.DeadlineExceeded, racersdk.ErrUnavailable},
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
				if tc.kind != nil {
					requireError(t, err, tc.kind)
				} else if err == nil || errors.Is(err, racersdk.ErrUnavailable) {
					t.Fatalf("expected unclassified origin failure, got %v", err)
				}

				if strings.Contains(err.Error(), "private-token") {
					t.Fatal("error leaked cause")
				}

				if upstream.heads.Load() != 0 || upstream.pulls.Load() != 1 {
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
				if err == nil || errors.Is(err, racersdk.ErrUnavailable) {
					t.Fatalf("expected unclassified invalid origin response, got %v", err)
				}

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

	value := adapterGet(t, client, testRef(), "")

	n, err := io.Copy(io.Discard, value)
	if err == nil || n >= size {
		t.Fatalf("missing continuation succeeded: bytes=%d, error=%v", n, err)
	}

	if upstream.heads.Load() != 0 || upstream.pulls.Load() != 2 {
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

	t.Run("client lifetime", func(t *testing.T) {
		client := racersdktest.NewClient(t, Origin(testConfig(), upstream))
		adapterGet(t, client, testRef(), "")
	})

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

	metadata, body, err := open(context.Background(), upstream, testRef(), racersdk.OriginRequest{Head: true})
	if err != nil || metadata.Size != 123 || !validRacerMetadata(metadata, testRef().Digest) || body != nil || upstream.pulls.Load() != 0 {
		t.Fatalf("HEAD result: %+v, body=%v, error=%v", metadata, body, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = open(ctx, upstream, testRef(), racersdk.OriginRequest{Head: true})
	requireError(t, err, context.Canceled)

	if !errors.Is(err, context.Canceled) || upstream.heads.Load() != 1 {
		t.Fatal("canceled context was lost or reached upstream")
	}
}

func TestRegistryOriginManifestContinuation(t *testing.T) {
	data := bytes.Repeat([]byte("m"), int(racersdk.PageSize)+23)

	var continuations, heads atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))

		if r.Method == http.MethodHead {
			heads.Add(1)
			return
		}

		offset := 0

		end := int(racersdk.PageSize) - 1
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, 2*racersdk.PageSize-1) || r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, len(data)-1) {
			offset = int(racersdk.PageSize)
			end = len(data) - 1

			continuations.Add(1)
		} else if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(end-offset+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[offset : end+1])
	}))
	defer server.Close()

	client := adapterRegistryClient(t, server.URL)
	ref := testRef()
	ref.Digest = digestOf(data)

	value := adapterGet(t, client, ref, "")

	if value.Metadata().ContentType != "application/vnd.oci.image.manifest.v1+json; charset=utf-8" {
		t.Fatalf("manifest media type lost: %q", value.Metadata().ContentType)
	}

	got, err := io.ReadAll(value)
	if err != nil || !bytes.Equal(got, data) || digestOf(got) != ref.Digest || continuations.Load() != 1 || heads.Load() != 0 {
		t.Fatalf("manifest continuation: bytes=%d, ranges=%d, error=%v", len(got), continuations.Load(), err)
	}
}

type boundedFixture struct {
	*testUpstream
	read func(context.Context, ifaces.OriginRef, int64) (io.ReadCloser, int64, string, error)
}

func (f boundedFixture) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	return f.read(ctx, ref, length)
}

func TestOriginPinnedMetadataConsistency(t *testing.T) {
	for _, change := range []string{"size", "content type"} {
		t.Run(change, func(t *testing.T) {
			size := int64(racersdk.PageSize) + 3
			fixture := boundedFixture{testUpstream: &testUpstream{}, read: func(_ context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
				contentType := "application/octet-stream"
				total := size

				if ref.Offset > 0 {
					if change == "size" {
						total++
					} else {
						contentType = "application/vnd.oci.image.index.v1+json"
					}
				}

				return io.NopCloser(io.LimitReader(zeroReader{}, min(length, total-ref.Offset))), total, contentType, nil
			}}
			client := fakeClient(t, Origin(testConfig(), fixture))

			value := adapterGet(t, client, testRef(), "")

			n, err := io.Copy(io.Discard, value)
			if err == nil || n >= size {
				t.Fatalf("changed pinned metadata accepted: bytes=%d err=%v", n, err)
			}

			if fixture.heads.Load() != 0 {
				t.Fatal("page issued HEAD")
			}
		})
	}
}

func TestOriginCredentialsDoNotChangeIdentity(t *testing.T) {
	a := requestFor(t, testRef(), "Bearer first")

	b := requestFor(t, testRef(), "Bearer second")
	if a.Key != b.Key || a.Metadata != b.Metadata {
		t.Fatal("credentials affected cache identity")
	}
}

func TestRegistryOriginDistributionManifest(t *testing.T) {
	for _, kind := range []ifaces.OriginRefKind{ifaces.KindManifest, ifaces.KindBlob} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/truncated=%v", kind, truncated), func(t *testing.T) {
				const (
					data        = `{"schemaVersion":2,"manifests":[]}`
					contentType = "application/vnd.oci.image.index.v1+json"
				)

				var gets, heads atomic.Int64

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodHead {
						heads.Add(1)
						w.WriteHeader(405)

						return
					}

					gets.Add(1)

					if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1) {
						t.Errorf("Range=%q", r.Header.Get("Range"))
					}

					if strings.Contains(r.URL.Path, "/blobs/") {
						w.WriteHeader(404)
						return
					}

					w.Header().Set("Content-Type", contentType)
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.WriteHeader(200)

					payload := data
					if truncated {
						payload = data[:len(data)-1]
					}

					_, _ = io.WriteString(w, payload)
				}))
				defer srv.Close()

				client := adapterRegistryClient(t, srv.URL)
				ref := testRef()
				ref.Kind, ref.Digest = kind, digestOf([]byte(data))

				value := adapterGet(t, client, ref, "")

				got, err := io.ReadAll(value)
				if truncated {
					if err == nil {
						t.Fatal("truncated manifest accepted")
					}
				} else if err != nil || string(got) != data || digestOf(got) != ref.Digest {
					t.Fatalf("body=%q error=%v", got, err)
				}

				metadata := value.Metadata()

				wantGets := int64(1)
				if kind == ifaces.KindBlob {
					wantGets++
				}

				if metadata.Size != int64(len(data)) || metadata.ContentType != contentType || gets.Load() != wantGets || heads.Load() != 0 {
					t.Fatalf("metadata=%+v gets=%d heads=%d", metadata, gets.Load(), heads.Load())
				}
			})
		}
	}
}

func TestRegistryOriginRejectsDelegatedHTTPBeforeIO(t *testing.T) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(401) }))
	defer srv.Close()

	client := adapterRegistryClient(t, srv.URL)
	_, err := client.Get(t.Context(), requestFor(t, testRef(), "Bearer public-fixture"))
	requireError(t, err, racersdk.ErrUnauthorized)

	if hits.Load() != 0 {
		t.Fatal("delegated HTTP request reached origin")
	}
}
