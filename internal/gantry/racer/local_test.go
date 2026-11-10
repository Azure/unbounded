// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/contentstore"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// localFixture writes data into a fake containerd content root and returns a
// LocalStore over it plus a matching reference.
func localFixture(t *testing.T, data []byte, kind ifaces.OriginRefKind) (LocalStore, ifaces.OriginRef) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "blobs", "sha256")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ref := testRef()
	ref.Kind = kind
	ref.Digest = digestOf(data)

	if err := os.WriteFile(filepath.Join(dir, ref.Digest.Hex()), data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := contentstore.New(root)
	if err != nil {
		t.Fatal(err)
	}

	return ContentStore(store), ref
}

func localOrigin(t *testing.T, store LocalStore, upstream *testUpstream) racersdk.Origin {
	t.Helper()

	return Origin(testConfig(), upstream, WithLocalStore(store), func(o *originOptions) {
		o.localOnly = func(racersdk.OriginRequest) bool { return true }
	})
}

func TestIsLocalOnlyDefaultsFalse(t *testing.T) {
	if isLocalOnly(racersdk.OriginRequest{}) {
		t.Fatal("isLocalOnly must stay false until racersdk carries LocalOnly")
	}
}

func TestLocalOnlyServesPages(t *testing.T) {
	data := []byte("0123456789abcdef")
	store, ref := localFixture(t, data, ifaces.KindBlob)
	upstream := &testUpstream{}
	origin := localOrigin(t, store, upstream)
	tag := `"` + ref.Digest.String() + `"`

	for _, tc := range []struct {
		offset, length int64
		want           string
	}{
		{0, racersdk.PageSize, string(data)},
		{4, 6, "456789"},
		{10, 100, "abcdef"},
	} {
		metadata, body, err := origin(t.Context(), racersdk.OriginRequest{
			Request: requestFor(t, ref, ""), ETag: tag, Offset: tc.offset, Length: tc.length,
		})
		if err != nil {
			t.Fatal(err)
		}

		if metadata.Size != int64(len(data)) || metadata.ETag != tag || metadata.ContentType != "application/octet-stream" || metadata.ExpiresAt.IsZero() {
			t.Fatalf("metadata = %+v", metadata)
		}

		got, err := io.ReadAll(body)
		if err != nil || string(got) != tc.want {
			t.Fatalf("body = %q, %v; want %q", got, err, tc.want)
		}

		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if upstream.heads.Load()+upstream.pulls.Load() != 0 {
		t.Fatal("local-only request reached the registry")
	}
}

func TestLocalOnlyHeadAndPastEnd(t *testing.T) {
	data := []byte("payload")
	store, ref := localFixture(t, data, ifaces.KindConfig)
	upstream := &testUpstream{}
	origin := localOrigin(t, store, upstream)

	for _, request := range []racersdk.OriginRequest{
		{Request: requestFor(t, ref, ""), Head: true},
		{Request: requestFor(t, ref, ""), Offset: racersdk.PageSize, Length: racersdk.PageSize},
		{Request: requestFor(t, ref, "")},
	} {
		metadata, body, err := origin(t.Context(), request)
		if err != nil || body != nil || metadata.Size != int64(len(data)) || !validRacerMetadata(metadata, ref.Digest) {
			t.Fatalf("%v: metadata=%+v body=%v err=%v", request, metadata, body, err)
		}
	}

	if upstream.heads.Load()+upstream.pulls.Load() != 0 {
		t.Fatal("local-only request reached the registry")
	}
}

func TestLocalOnlyMisses(t *testing.T) {
	data := []byte("present")
	store, ref := localFixture(t, data, ifaces.KindBlob)
	missing := ref
	missing.Digest = digestOf([]byte("absent"))

	for name, tc := range map[string]struct {
		store LocalStore
		ref   ifaces.OriginRef
	}{
		"missing":  {store, missing},
		"no store": {nil, ref},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &testUpstream{}

			_, body, err := localOrigin(t, tc.store, upstream)(t.Context(), racersdk.OriginRequest{
				Request: requestFor(t, tc.ref, ""), Length: racersdk.PageSize,
			})
			requireError(t, err, racersdk.ErrNotFound)

			if body != nil || upstream.heads.Load()+upstream.pulls.Load() != 0 {
				t.Fatal("local miss returned a body or reached the registry")
			}
		})
	}

	_, _, err := localOrigin(t, store, &testUpstream{})(t.Context(), racersdk.OriginRequest{
		Request: requestFor(t, ref, ""), ETag: `"sha256:other"`, Length: racersdk.PageSize,
	})
	requireError(t, err, racersdk.ErrVersionMismatch)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = localOrigin(t, store, &testUpstream{})(ctx, racersdk.OriginRequest{Request: requestFor(t, ref, ""), Length: racersdk.PageSize})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLocalOnlyManifestMediaType(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	store, ref := localFixture(t, manifest, ifaces.KindManifest)

	metadata, body, err := localOrigin(t, store, &testUpstream{})(t.Context(), racersdk.OriginRequest{
		Request: requestFor(t, ref, ""), Length: racersdk.PageSize,
	})
	if err != nil || metadata.ContentType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}

	_ = body.Close() //nolint:errcheck // test cleanup

	untyped, untypedRef := localFixture(t, []byte(`{"schemaVersion":2}`), ifaces.KindManifest)

	_, _, err = localOrigin(t, untyped, &testUpstream{})(t.Context(), racersdk.OriginRequest{
		Request: requestFor(t, untypedRef, ""), Length: racersdk.PageSize,
	})
	requireError(t, err, racersdk.ErrNotFound)
}

func TestNonLocalRequestIgnoresLocalStore(t *testing.T) {
	data := []byte("registry only")
	store, ref := localFixture(t, data, ifaces.KindBlob)
	upstream := &testUpstream{head: func(context.Context, ifaces.OriginRef) (int64, string, error) {
		return 13, "application/x-registry", nil
	}}

	metadata, _, err := Origin(testConfig(), upstream, WithLocalStore(store))(t.Context(), racersdk.OriginRequest{
		Request: requestFor(t, ref, ""), Head: true,
	})
	if err != nil || metadata.ContentType != "application/octet-stream" || metadata.Size != 13 || upstream.heads.Load() != 1 {
		t.Fatalf("metadata=%+v err=%v heads=%d", metadata, err, upstream.heads.Load())
	}
}

func TestLocalAndRegistryBlobMetadataAgree(t *testing.T) {
	data := []byte("same bytes")
	store, ref := localFixture(t, data, ifaces.KindBlob)
	upstream := &testUpstream{head: func(context.Context, ifaces.OriginRef) (int64, string, error) {
		return int64(len(data)), "application/vnd.oci.image.layer.v1.tar+gzip", nil
	}}
	origin := Origin(testConfig(), upstream, WithLocalStore(store))

	remote, _, err := origin(t.Context(), racersdk.OriginRequest{Request: requestFor(t, ref, ""), Head: true})
	if err != nil {
		t.Fatal(err)
	}

	local, body, found, err := serveLocal(t.Context(), store, ref, racersdk.OriginRequest{
		Request: requestFor(t, ref, ""), Head: true,
	}, true)
	if body != nil {
		_ = body.Close()
	}

	if err != nil || !found {
		t.Fatalf("local head: found=%v err=%v", found, err)
	}

	if remote.Size != local.Size || remote.ETag != local.ETag || remote.ContentType != local.ContentType {
		t.Fatalf("registry %+v and local %+v metadata disagree", remote, local)
	}
}
