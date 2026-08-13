// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package snapshotter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
)

// The whole point of this snapshotter is a contract inside containerd: when
// Prepare answers ErrAlreadyExists and a Stat of the chain ID then succeeds,
// the unpacker treats the layer as done and never fetches or applies it. That
// behaviour lives in containerd's core/unpack package, not in ours, so nothing
// we write about our own code proves it still holds. These tests drive the real
// unpacker against the real snapshotter so that a containerd bump which changes
// the contract fails here rather than silently turning the whole design into an
// expensive no-op.

// errNoSegmentHere is what a mapper reports when the catalog names a segment
// this node does not export.
var errNoSegmentHere = errors.New("segment 1 is not exported on this node")

// memLabels is an in-memory label store. The local content store is immutable
// without one, and the unpacker records its garbage collection reference as a
// label on the image config once every layer is accounted for.
type memLabels struct {
	mu     sync.Mutex
	labels map[digest.Digest]map[string]string
}

func newLabels() *memLabels {
	return &memLabels{labels: map[digest.Digest]map[string]string{}}
}

func (l *memLabels) Get(d digest.Digest) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return maps.Clone(l.labels[d]), nil
}

func (l *memLabels) Set(d digest.Digest, labels map[string]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.labels[d] = maps.Clone(labels)

	return nil
}

func (l *memLabels) Update(d digest.Digest, update map[string]string) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	current := l.labels[d]
	if current == nil {
		current = map[string]string{}
	}

	for k, v := range update {
		if v == "" {
			delete(current, k)

			continue
		}

		current[k] = v
	}

	l.labels[d] = current

	return maps.Clone(current), nil
}

// newContentStore builds a content store the unpacker can label.
func newContentStore(t *testing.T) content.Store {
	t.Helper()

	cs, err := local.NewLabeledStore(t.TempDir(), newLabels())
	if err != nil {
		t.Fatalf("content store: %v", err)
	}

	return cs
}

// fixture is a small OCI image laid out in a content store.
type fixture struct {
	manifest ocispec.Descriptor
	layers   []ocispec.Descriptor
	diffIDs  []digest.Digest
	chainIDs []digest.Digest
}

// buildImage writes a layers-deep image into cs and returns its descriptors.
// The layer payloads are placeholders: no test here ever extracts one, because
// either the snapshotter claims the layer or the applier is a stub.
func buildImage(t *testing.T, ctx context.Context, cs content.Store, layers int) fixture {
	t.Helper()

	f := fixture{}

	for i := range layers {
		payload := fmt.Appendf(nil, "layer-%d", i)
		desc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    digest.FromBytes(payload),
			Size:      int64(len(payload)),
		}
		writeBlob(t, ctx, cs, payload, desc)

		f.layers = append(f.layers, desc)
		f.diffIDs = append(f.diffIDs, digest.FromString(fmt.Sprintf("diff-%d", i)))
	}

	f.chainIDs = identity.ChainIDs(append([]digest.Digest(nil), f.diffIDs...))

	config := struct {
		Architecture string         `json:"architecture"`
		OS           string         `json:"os"`
		RootFS       ocispec.RootFS `json:"rootfs"`
	}{
		Architecture: "amd64",
		OS:           "linux",
		RootFS:       ocispec.RootFS{Type: "layers", DiffIDs: f.diffIDs},
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configBytes),
		Size:      int64(len(configBytes)),
	}
	writeBlob(t, ctx, cs, configBytes, configDesc)

	manifestBytes, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    f.layers,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	f.manifest = ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
	}
	writeBlob(t, ctx, cs, manifestBytes, f.manifest)

	return f
}

func writeBlob(t *testing.T, ctx context.Context, cs content.Store, payload []byte, desc ocispec.Descriptor) {
	t.Helper()

	if err := content.WriteBlob(ctx, cs, desc.Digest.String(), bytes.NewReader(payload), desc); err != nil {
		t.Fatalf("write blob %s: %v", desc.Digest, err)
	}
}

// countingHandler records every layer descriptor the unpacker asks to fetch.
type countingHandler struct {
	inner images.Handler

	mu      sync.Mutex
	fetched []digest.Digest
}

func (h *countingHandler) Handle(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	if images.IsLayerType(desc.MediaType) {
		h.mu.Lock()
		h.fetched = append(h.fetched, desc.Digest)
		h.mu.Unlock()
	}

	return h.inner.Handle(ctx, desc)
}

func (h *countingHandler) all() []digest.Digest {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]digest.Digest(nil), h.fetched...)
}

// stubApplier stands in for the diff service. It records what it was asked to
// extract and reports the diff ID the unpacker expects, so a test can run the
// miss path without mounting anything.
type stubApplier struct {
	diffIDs map[digest.Digest]digest.Digest

	mu      sync.Mutex
	applied []digest.Digest
}

func (a *stubApplier) Apply(_ context.Context, desc ocispec.Descriptor, _ []mount.Mount, _ ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	a.mu.Lock()
	a.applied = append(a.applied, desc.Digest)
	a.mu.Unlock()

	diffID, ok := a.diffIDs[desc.Digest]
	if !ok {
		return ocispec.Descriptor{}, fmt.Errorf("no diff id for %s", desc.Digest)
	}

	return ocispec.Descriptor{Digest: diffID}, nil
}

func (a *stubApplier) all() []digest.Digest {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]digest.Digest(nil), a.applied...)
}

// unpackImage runs containerd's unpacker over f using h's snapshotter.
func unpackImage(t *testing.T, h *harness, cs content.Store, f fixture) (*countingHandler, *stubApplier) {
	t.Helper()

	applier := &stubApplier{diffIDs: map[digest.Digest]digest.Digest{}}
	for i, layer := range f.layers {
		applier.diffIDs[layer.Digest] = f.diffIDs[i]
	}

	handler := &countingHandler{inner: images.ChildrenHandler(cs)}

	u, err := unpack.NewUnpacker(h.ctx, cs, unpack.WithUnpackPlatform(unpack.Platform{
		SnapshotterKey: "gantry",
		Snapshotter:    h.sn,
		Applier:        applier,
	}))
	if err != nil {
		t.Fatalf("NewUnpacker: %v", err)
	}

	if err := images.Walk(h.ctx, u.Unpack(handler), f.manifest); err != nil {
		t.Fatalf("walk: %v", err)
	}

	if _, err := u.Wait(); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	return handler, applier
}

// publishLayer records a layer in the fake catalog the way another node's
// ingest would, so this node can adopt it.
func publishLayer(t *testing.T, h *harness, f fixture, i int) {
	t.Helper()

	chain, err := catalog.ParseDigest(f.chainIDs[i].String())
	if err != nil {
		t.Fatalf("parse chain id: %v", err)
	}

	diffID, err := catalog.ParseDigest(f.diffIDs[i].String())
	if err != nil {
		t.Fatalf("parse diff id: %v", err)
	}

	h.cat.publish(chain, diffID, addrOf(uint32(i)))
}

func TestUnpackerSkipsEveryLayerTheClusterAlreadyHas(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	cs := newContentStore(t)

	f := buildImage(t, h.ctx, cs, 3)
	for i := range f.layers {
		publishLayer(t, h, f, i)
	}

	handler, applier := unpackImage(t, h, cs, f)

	if got := handler.all(); len(got) != 0 {
		t.Fatalf("the unpacker fetched %d layers the cluster already had: %v", len(got), got)
	}

	if got := applier.all(); len(got) != 0 {
		t.Fatalf("the unpacker extracted %d layers the cluster already had: %v", len(got), got)
	}

	// Every chain ID has to be readable afterwards, otherwise containerd has
	// no snapshot to run the container from.
	for i, chain := range f.chainIDs {
		if _, err := h.sn.Stat(h.ctx, chain.String()); err != nil {
			t.Fatalf("layer %d chain %s: %v", i, chain, err)
		}
	}
}

func TestUnpackerFetchesFromTheFirstLayerItIsMissing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	cs := newContentStore(t)

	f := buildImage(t, h.ctx, cs, 3)

	// The cluster has the base of the image but not its top layer, which is
	// the common case for an image whose last layer changes every build.
	publishLayer(t, h, f, 0)
	publishLayer(t, h, f, 1)

	handler, applier := unpackImage(t, h, cs, f)

	want := []digest.Digest{f.layers[2].Digest}

	if got := handler.all(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("fetched %v, want %v", got, want)
	}

	if got := applier.all(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("extracted %v, want %v", got, want)
	}

	for i, chain := range f.chainIDs {
		if _, err := h.sn.Stat(h.ctx, chain.String()); err != nil {
			t.Fatalf("layer %d chain %s: %v", i, chain, err)
		}
	}
}

func TestUnpackerFallsBackWhenTheNodeCannotMap(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	cs := newContentStore(t)

	f := buildImage(t, h.ctx, cs, 2)
	for i := range f.layers {
		publishLayer(t, h, f, i)
	}

	// The catalog says the cluster has both layers, but this node cannot
	// reach the segment holding them. Adoption has to decline and the image
	// has to unpack locally instead of failing.
	h.m.err = errNoSegmentHere

	handler, applier := unpackImage(t, h, cs, f)

	if got := handler.all(); len(got) != len(f.layers) {
		t.Fatalf("fetched %v, want every layer", got)
	}

	if got := applier.all(); len(got) != len(f.layers) {
		t.Fatalf("extracted %v, want every layer", got)
	}

	for i, chain := range f.chainIDs {
		if _, err := h.sn.Stat(h.ctx, chain.String()); err != nil {
			t.Fatalf("layer %d chain %s: %v", i, chain, err)
		}
	}
}
