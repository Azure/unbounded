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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func testRef() ifaces.OriginRef {
	return ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: digestOf([]byte("fixture"))}
}

func digestOf(data []byte) digest.Digest {
	return digest.MustParse(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

func testConfig() *config.Config {
	return &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example", NSAlias: "alias.example"}}}
}

func requestFor(t *testing.T, ref ifaces.OriginRef, authorization string) racersdk.Request {
	t.Helper()

	req, err := Request(ref, authorization)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func TestRacerSDKConfigWiring(t *testing.T) {
	for _, limits := range []int{0, 3, 64} {
		c := config.NewDefault()
		c.RacerMaxConnections = limits
		c.RacerOriginConcurrentRequests = limits

		client, callback := ClientConfig(c, CacheName), OriginConfig(c, CacheName)
		if client.Cache != "gantry" || callback.Cache != "gantry" || client.MaxConnections != limits || callback.MaxConcurrentRequests != limits {
			t.Fatalf("client=%+v origin=%+v", client, callback)
		}

		sdk, err := racersdk.NewClient(client)
		if err != nil {
			t.Fatal(err)
		}

		if err := sdk.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if SocketPath("origin") != "/run/racer/gantry/origin/socket" || SocketPath("client") != "/run/racer/gantry/client/socket" {
		t.Fatal("unexpected canonical sockets")
	}
}

func TestRequest(t *testing.T) {
	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindConfig, ifaces.KindManifest} {
		ref := testRef()
		ref.Kind = kind
		req := requestFor(t, ref, "Bearer private-token")

		decoded, err := decodeReference(req.Key, req.Metadata)
		if err != nil || decoded != ref || req.Key.String() != ref.Digest.Hex() || req.Authorization != "Bearer private-token" {
			t.Fatalf("round trip = %+v, %v", decoded, err)
		}

		if strings.Contains(req.Metadata, "private-token") || strings.Contains(fmt.Sprintf("%#v", req), "private-token") {
			t.Fatal("credential leaked")
		}

		other := requestFor(t, ref, "Bearer other")
		if other.Key != req.Key || other.Metadata != req.Metadata {
			t.Fatal("credentials changed cache identity")
		}
	}
}

func TestRequestRejectsInvalidInput(t *testing.T) {
	for _, change := range []func(*ifaces.OriginRef){
		func(r *ifaces.OriginRef) { r.Digest = digest.Digest{} },
		func(r *ifaces.OriginRef) { r.Digest, _ = digest.Parse("sha512:" + strings.Repeat("a", 128)) },
		func(r *ifaces.OriginRef) { r.Offset = 1 },
		func(r *ifaces.OriginRef) { r.Offset = -1 },
		func(r *ifaces.OriginRef) { r.Kind = 99 },
		func(r *ifaces.OriginRef) { r.Repository = "../secret" },
		func(r *ifaces.OriginRef) { r.Registry = "user:secret@registry.example" },
		func(r *ifaces.OriginRef) { r.Registry = "https://registry.example" },
		func(r *ifaces.OriginRef) { r.Registry += "?token=secret" },
	} {
		ref := testRef()
		change(&ref)

		if _, err := Request(ref, ""); !errors.Is(err, racersdk.ErrInvalidRequest) {
			t.Fatalf("invalid reference accepted: %v", err)
		}
	}

	for _, auth := range []string{"Digest private", "Basic invalid", "Bearer", "Bearer a\r\nb", "Bearer " + strings.Repeat("a", 8192)} {
		if _, err := Request(testRef(), auth); err == nil {
			t.Fatal("accepted invalid authorization")
		}
	}
}

type testUpstream struct {
	head func(context.Context, ifaces.OriginRef) (int64, string, error)
	pull func(context.Context, ifaces.OriginRef, int64) (io.ReadCloser, int64, string, error)
}

func (u testUpstream) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	return u.head(ctx, ref)
}

func (u testUpstream) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	return u.pull(ctx, ref, length)
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
		req := racersdk.OriginRequest{Request: requestFor(t, testRef(), "")}
		req.Metadata = metadata

		_, _, err := Origin(testConfig(), testUpstream{})(t.Context(), req)
		if !errors.Is(err, racersdk.ErrInvalidRequest) {
			t.Fatalf("untrusted metadata: %v", err)
		}
	}
}

func TestOriginPagesCredentialsAndHead(t *testing.T) {
	for _, size := range []int{0, 17, int(racersdk.PageSize), 2*int(racersdk.PageSize) + 37} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{0x5a}, size)
			ref := testRef()
			ref.Registry, ref.Digest = "alias.example", digestOf(data)

			var heads, pulls atomic.Int32

			check := func(ctx context.Context, got ifaces.OriginRef) {
				got.Offset = 0
				if got != ref || registryauth.Authorization(ctx) != "Bearer delegated" {
					t.Error("incorrect reference or credential")
				}
			}
			upstream := testUpstream{
				head: func(ctx context.Context, got ifaces.OriginRef) (int64, string, error) {
					check(ctx, got)
					heads.Add(1)

					return int64(size), "application/octet-stream", nil
				},
				pull: func(ctx context.Context, got ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
					check(ctx, got)
					pulls.Add(1)

					if got.Offset < 0 || got.Offset > int64(size) || length > racersdk.PageSize {
						return nil, 0, "", errors.New("invalid bounds")
					}

					return io.NopCloser(bytes.NewReader(data[got.Offset:min(int64(size), got.Offset+length)])), int64(size), "application/octet-stream", nil
				},
			}
			client := racersdktest.NewClient(t, Origin(testConfig(), upstream))
			req := requestFor(t, ref, "Bearer delegated")

			value, err := client.Get(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer value.Close()

			got, err := io.ReadAll(value)
			if err != nil || !bytes.Equal(got, data) || heads.Load() != 0 || pulls.Load() != int32(max(1, (size+int(racersdk.PageSize)-1)/int(racersdk.PageSize))) {
				t.Fatalf("bytes=%d heads=%d pulls=%d err=%v", len(got), heads.Load(), pulls.Load(), err)
			}

			metadata, err := client.Stat(t.Context(), req)
			if err != nil || metadata.Size != int64(size) || !validRacerMetadata(metadata, ref.Digest) || heads.Load() != 1 {
				t.Fatalf("HEAD: %+v %v", metadata, err)
			}
		})
	}
}

func TestOriginPinErrorsAndEmptyRange(t *testing.T) {
	ref := testRef()
	for _, head := range []bool{true, false} {
		_, _, err := open(t.Context(), testUpstream{}, ref, racersdk.OriginRequest{Head: head, ETag: `"wrong"`})
		if !errors.Is(err, racersdk.ErrVersionMismatch) {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		class  ifaces.FailureClass
		status int
		want   error
	}{
		{ifaces.FailureAuth, 401, racersdk.ErrUnauthorized},
		{ifaces.FailureAuth, 403, racersdk.ErrForbidden},
		{ifaces.FailureNotFound, 404, racersdk.ErrNotFound},
		{ifaces.FailureRateLimited, 429, racersdk.ErrUnavailable},
		{ifaces.FailureTransient, 503, racersdk.ErrUnavailable},
	} {
		err := classifyError(&ifaces.OriginError{Class: test.class, StatusCode: test.status})
		if !errors.Is(err, test.want) {
			t.Fatalf("classification: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, _, err := open(ctx, testUpstream{}, ref, racersdk.OriginRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	for _, size := range []int64{-1, 0, 3} {
		upstream := testUpstream{pull: func(context.Context, ifaces.OriginRef, int64) (io.ReadCloser, int64, string, error) {
			return nil, size, "", nil
		}}

		_, body, err := open(t.Context(), upstream, ref, racersdk.OriginRequest{Length: 1})
		if body != nil || (err != nil) != (size != 0) {
			t.Fatalf("size=%d body=%v err=%v", size, body, err)
		}
	}
}

func TestOriginBoundedRegistryResponse(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"manifests":[]}`)
	ref := testRef()
	ref.Digest, ref.Kind = digestOf(data), ifaces.KindManifest

	var gets atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)

		if r.Method != http.MethodGet || r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1) {
			t.Errorf("unbounded or redundant request: %s %v", r.Method, r.Header)
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.UpstreamRegistries[0].Endpoint = server.URL
	cfg.UpstreamRegistries[0].CredentialsPath = filepath.Join(t.TempDir(), "missing")

	upstream, err := origin.New(cfg, origin.WithDelegatedCredentialsOnly())
	if err != nil {
		t.Fatal(err)
	}

	client := racersdktest.NewClient(t, Origin(cfg, upstream))

	value, err := client.Get(t.Context(), requestFor(t, ref, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	got, err := io.ReadAll(value)
	if err != nil || !bytes.Equal(got, data) || gets.Load() != 1 || value.Metadata().ContentType != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("registry response: %q %v gets=%d", got, err, gets.Load())
	}

	if _, err := client.Get(t.Context(), requestFor(t, ref, "Bearer delegated")); !errors.Is(err, racersdk.ErrUnauthorized) || gets.Load() != 1 {
		t.Fatalf("delegated credential reached plaintext registry: %v", err)
	}
}

func TestPrepareRacerOriginDirectory(t *testing.T) {
	root := t.TempDir()
	want := errors.New("serve result")

	err := serveOriginAt(t.Context(), racersdk.OriginConfig{Cache: CacheName}, nil, root,
		func(_ context.Context, cfg racersdk.OriginConfig, _ racersdk.Origin) error {
			if !cfg.RecoverStaleSocket || cfg.Cache != "gantry" {
				t.Fatal("missing recovery or cache")
			}

			return want
		})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}

	path := filepath.Join(root, "gantry", "origin")
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := prepareRacerOriginDirectory(root); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("changed existing permissions: %v %v", info, err)
	}

	if _, err := os.Stat(filepath.Join(root, "gantry", "client")); !os.IsNotExist(err) {
		t.Fatal("created client endpoint")
	}

	for _, component := range []string{"mount", "gantry", "origin"} {
		for _, kind := range []string{"file", "symlink", "dangling"} {
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
				if kind == "dangling" {
					target = filepath.Join(target, "missing")
				}

				var err error
				if kind == "file" {
					err = os.WriteFile(path, []byte("preserve"), 0o600)
				} else {
					err = os.Symlink(target, path)
				}

				if err != nil {
					t.Fatal(err)
				}

				if err := prepareRacerOriginDirectory(root); err == nil {
					t.Fatal("accepted unsafe path")
				}
			})
		}
	}

	if err := prepareRacerOriginDirectory(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("created missing mount: %v", err)
	}
}

func TestOriginExpiry(t *testing.T) {
	before := time.Now()
	upstream := testUpstream{head: func(context.Context, ifaces.OriginRef) (int64, string, error) { return 3, "", nil }}

	metadata, _, err := open(t.Context(), upstream, testRef(), racersdk.OriginRequest{Head: true})
	if err != nil || metadata.ExpiresAt.Before(before.Add(metadataTTL-time.Millisecond)) || metadata.ExpiresAt.After(time.Now().Add(metadataTTL)) {
		t.Fatalf("expiry=%v err=%v", metadata.ExpiresAt, err)
	}
}
