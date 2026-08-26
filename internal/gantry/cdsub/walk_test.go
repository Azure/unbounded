// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Tests for the platform-agnostic walk/inventory helpers. Uses an
// in-memory fake content store so the suite runs on darwin without a
// containerd socket.

package cdsub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	cerrdefs "github.com/containerd/errdefs"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeContentStore is the minimum content.Store implementation that
// satisfies walkBlobs + Inventory consumers (images.Walk + images.
// Children + ContentStore.Info/Walk/ReaderAt). All other methods panic
// because the helpers under test must never call them.
type fakeContentStore struct {
	blobs map[godigest.Digest][]byte
}

func newFakeStore() *fakeContentStore {
	return &fakeContentStore{blobs: map[godigest.Digest][]byte{}}
}

// put records a blob and returns its descriptor with the provided
// media type so manifests/configs can be assembled by hand in tests.
func (f *fakeContentStore) put(mt string, payload []byte) ocispec.Descriptor {
	d := godigest.FromBytes(payload)
	f.blobs[d] = payload

	return ocispec.Descriptor{MediaType: mt, Digest: d, Size: int64(len(payload))}
}

// putJSON marshals v to JSON, stores it, and returns the descriptor.
func (f *fakeContentStore) putJSON(t *testing.T, mt string, v any) ocispec.Descriptor {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}

	return f.put(mt, b)
}

func (f *fakeContentStore) Info(_ context.Context, dgst godigest.Digest) (content.Info, error) {
	b, ok := f.blobs[dgst]
	if !ok {
		return content.Info{}, cerrdefs.ErrNotFound
	}

	return content.Info{Digest: dgst, Size: int64(len(b)), CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}

func (f *fakeContentStore) Update(_ context.Context, _ content.Info, _ ...string) (content.Info, error) {
	panic("fakeContentStore.Update not implemented")
}

func (f *fakeContentStore) Walk(_ context.Context, fn content.WalkFunc, _ ...string) error {
	// Deterministic ordering keeps tests stable.
	keys := make([]godigest.Digest, 0, len(f.blobs))
	for d := range f.blobs {
		keys = append(keys, d)
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	for _, d := range keys {
		if err := fn(content.Info{Digest: d, Size: int64(len(f.blobs[d]))}); err != nil {
			return err
		}
	}

	return nil
}

func (f *fakeContentStore) Delete(_ context.Context, _ godigest.Digest) error {
	panic("fakeContentStore.Delete not implemented")
}

func (f *fakeContentStore) ReaderAt(_ context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	b, ok := f.blobs[desc.Digest]
	if !ok {
		return nil, cerrdefs.ErrNotFound
	}

	return &fakeReaderAt{data: b}, nil
}

func (f *fakeContentStore) Status(_ context.Context, _ string) (content.Status, error) {
	panic("fakeContentStore.Status not implemented")
}

func (f *fakeContentStore) ListStatuses(_ context.Context, _ ...string) ([]content.Status, error) {
	panic("fakeContentStore.ListStatuses not implemented")
}

func (f *fakeContentStore) Abort(_ context.Context, _ string) error {
	panic("fakeContentStore.Abort not implemented")
}

func (f *fakeContentStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	panic("fakeContentStore.Writer not implemented")
}

type fakeReaderAt struct {
	data []byte
}

func (r *fakeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}

	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (r *fakeReaderAt) Close() error { return nil }
func (r *fakeReaderAt) Size() int64  { return int64(len(r.data)) }

var (
	_ content.Store    = (*fakeContentStore)(nil)
	_ content.ReaderAt = (*fakeReaderAt)(nil)
)

// TestWalkBlobs_SimpleImage covers the common case: an image manifest
// whose config + layer descriptors are all present in the content store.
// Every digest in the subtree should be emitted exactly once.
func TestWalkBlobs_SimpleImage(t *testing.T) {
	store := newFakeStore()

	configDesc := store.put(ocispec.MediaTypeImageConfig, []byte(`{"arch":"amd64"}`))
	layerDesc := store.put(ocispec.MediaTypeImageLayerGzip, []byte("layer-bytes"))

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestDesc := store.putJSON(t, ocispec.MediaTypeImageManifest, manifest)

	got, err := walkBlobs(context.Background(), store, manifestDesc)
	if err != nil {
		t.Fatalf("walkBlobs: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("walkBlobs returned %d digests, want 3", len(got))
	}

	gotSet := map[string]struct{}{}
	for _, d := range got {
		gotSet[d.String()] = struct{}{}
	}

	for _, want := range []godigest.Digest{manifestDesc.Digest, configDesc.Digest, layerDesc.Digest} {
		if _, ok := gotSet[want.String()]; !ok {
			t.Errorf("missing digest %s in walk output", want)
		}
	}
}

// TestWalkBlobs_AbsentChildNotAdvertised verifies the plan-mandated
// invariant: a descriptor referenced by a manifest but NOT present in
// the content store must be silently skipped, while sibling subtrees
// that ARE present still produce announcements. Without this, the
// transfer endpoint would 404 on phantom DHT records.
func TestWalkBlobs_AbsentChildNotAdvertised(t *testing.T) {
	store := newFakeStore()

	configDesc := store.put(ocispec.MediaTypeImageConfig, []byte(`{"arch":"amd64"}`))
	presentLayer := store.put(ocispec.MediaTypeImageLayerGzip, []byte("present"))
	// Synthesize a layer descriptor whose blob is NOT in the store.
	missingLayer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    godigest.FromBytes([]byte("missing-but-referenced")),
		Size:      42,
	}

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{presentLayer, missingLayer},
	}
	manifestDesc := store.putJSON(t, ocispec.MediaTypeImageManifest, manifest)

	got, err := walkBlobs(context.Background(), store, manifestDesc)
	if err != nil {
		t.Fatalf("walkBlobs: %v", err)
	}

	gotSet := map[string]struct{}{}
	for _, d := range got {
		gotSet[d.String()] = struct{}{}
	}

	if _, ok := gotSet[missingLayer.Digest.String()]; ok {
		t.Errorf("missing layer %s was advertised despite being absent from the content store", missingLayer.Digest)
	}

	for _, want := range []godigest.Digest{manifestDesc.Digest, configDesc.Digest, presentLayer.Digest} {
		if _, ok := gotSet[want.String()]; !ok {
			t.Errorf("present digest %s missing from walk output", want)
		}
	}
}

// TestWalkBlobs_MultiArchIndexPartialPlatform models the CRI scenario:
// kubelet pulls only the platform-matching manifest from a multi-arch
// image index, so other-arch manifests are referenced by the index but
// absent from the content store. walkBlobs must still emit the index
// digest plus every digest in the present platform subtree, without
// erroring out.
func TestWalkBlobs_MultiArchIndexPartialPlatform(t *testing.T) {
	store := newFakeStore()

	// Platform-present subtree (amd64).
	amdConfig := store.put(ocispec.MediaTypeImageConfig, []byte(`{"arch":"amd64"}`))
	amdLayer := store.put(ocispec.MediaTypeImageLayerGzip, []byte("amd64-layer"))
	amdManifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    amdConfig,
		Layers:    []ocispec.Descriptor{amdLayer},
	}
	amdManifestDesc := store.putJSON(t, ocispec.MediaTypeImageManifest, amdManifest)

	// Synthesize an arm64 manifest descriptor whose content was never
	// pulled.
	arm64Desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    godigest.FromBytes([]byte("arm64-manifest-never-pulled")),
		Size:      99,
	}

	index := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amdManifestDesc, arm64Desc},
	}
	indexDesc := store.putJSON(t, ocispec.MediaTypeImageIndex, index)

	got, err := walkBlobs(context.Background(), store, indexDesc)
	if err != nil {
		t.Fatalf("walkBlobs: %v", err)
	}

	gotSet := map[string]struct{}{}
	for _, d := range got {
		gotSet[d.String()] = struct{}{}
	}

	for _, want := range []godigest.Digest{indexDesc.Digest, amdManifestDesc.Digest, amdConfig.Digest, amdLayer.Digest} {
		if _, ok := gotSet[want.String()]; !ok {
			t.Errorf("present digest %s missing from walk output", want)
		}
	}

	if _, ok := gotSet[arm64Desc.Digest.String()]; ok {
		t.Errorf("arm64 manifest digest %s was advertised despite being absent", arm64Desc.Digest)
	}
}

// TestWalkBlobs_UnsupportedAlgorithmSkipped verifies that a descriptor
// with a non-sha256 digest is skipped without aborting the walk.
// Currently gdigest.Parse only accepts sha256, so anything else MUST
// be silently dropped - the rest of the agent cannot operate on it.
func TestWalkBlobs_UnsupportedAlgorithmSkipped(t *testing.T) {
	store := newFakeStore()
	// Build a small synthetic manifest blob and register it under a
	// sha512-shaped digest (string-cast: no real sha512 computation
	// needed and no extra crypto import). gdigest.Parse rejects
	// non-sha256, so walkBlobs must skip the descriptor and produce
	// an empty output (the manifest's children can't be reached
	// because we never asked images.Children to dereference it via
	// its real sha256 digest).
	const sha512Hex = "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

	sha512Digest := godigest.Digest("sha512:" + sha512Hex)
	store.blobs[sha512Digest] = []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    sha512Digest,
		Size:      int64(len(store.blobs[sha512Digest])),
	}

	got, err := walkBlobs(context.Background(), store, manifestDesc)
	if err != nil {
		t.Fatalf("walkBlobs: %v", err)
	}

	for _, d := range got {
		if strings.HasPrefix(d.String(), "sha512") {
			t.Errorf("walkBlobs emitted non-sha256 digest %s", d)
		}
	}
}

// TestWalkBlobs_PropagatesNonNotFoundError ensures that a transport-
// level error from the content store surfaces; only NotFound is
// downgraded to a skip. Without this, a flaky containerd would silently
// produce truncated digest sets.
func TestWalkBlobs_PropagatesNonNotFoundError(t *testing.T) {
	flaky := &errorStore{
		fakeContentStore: newFakeStore(),
		failDigest:       godigest.FromBytes([]byte("trigger")),
		failErr:          errors.New("simulated transient store outage"),
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    flaky.put(ocispec.MediaTypeImageConfig, []byte("c")),
	}
	manifestDesc := flaky.putJSON(t, ocispec.MediaTypeImageManifest, manifest)
	// Force Info on the manifest digest to return our injected error.
	flaky.failDigest = manifestDesc.Digest

	if _, err := walkBlobs(context.Background(), flaky, manifestDesc); err == nil {
		t.Fatal("walkBlobs returned nil err, expected transient outage to propagate")
	}
}

// errorStore overrides Info on a single digest to inject a non-
// NotFound failure. Models containerd briefly returning a transport
// error (e.g. EOF, ConnectionReset). All other methods delegate to the
// embedded fake.
type errorStore struct {
	*fakeContentStore
	failDigest godigest.Digest
	failErr    error
}

func (e *errorStore) Info(ctx context.Context, dgst godigest.Digest) (content.Info, error) {
	if dgst == e.failDigest && e.failErr != nil {
		return content.Info{}, e.failErr
	}

	return e.fakeContentStore.Info(ctx, dgst)
}
