// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package snapshotter

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
)

// DefaultNamespace is the containerd namespace the kubelet's images live in.
// Ingest runs on its own goroutine long after the gRPC call that triggered it,
// so it cannot inherit a namespace from the caller's context.
const DefaultNamespace = "k8s.io"

// ingestReason says what should happen to a snapshot that was just committed.
type ingestReason int

const (
	// reasonIngest means the snapshot is an image layer this node can publish.
	reasonIngest ingestReason = iota
	// reasonSkip means the snapshot is not an image layer, or carries a label
	// that cannot be read. Either way there is nothing to publish and nothing
	// to complain about.
	reasonSkip
	// reasonNoAnnotations means the snapshot is an image layer but containerd
	// did not say which blob it came from. That only happens when the CRI
	// plugin is configured with disable_snapshot_annotations, which is a
	// misconfiguration this snapshotter cannot work around and cannot detect
	// any other way.
	reasonNoAnnotations
)

// ingestRequest builds an ingest request for a layer this node just unpacked,
// or explains why it cannot:
//
//   - a snapshot whose chain ID cannot be recovered is a container layer, not
//     an image layer;
//   - a missing diff ID means containerd did not unpack this snapshot from an
//     image layer;
//   - a missing compressed digest means containerd was configured with
//     disable_snapshot_annotations, so there is no way to find the layer tar in
//     the content store. The layer still works locally, it just never reaches
//     the cluster, and neither does any other layer this node ever unpacks.
//
// The chain ID comes from the snapshot ref label rather than from the name
// containerd commits under. A proxy snapshotter sits behind containerd's
// metadata store, which rewrites every key it passes down into
// "<namespace>/<sequence>/<key>", so the name here is never a bare digest. The
// label is passed through untouched, which is also what makes Prepare's catalog
// probe and this agree on what a layer is called.
func ingestRequest(name string, labels map[string]string) (ingest.Request, ingestReason) {
	ref := labels[LabelSnapshotRef]
	if ref == "" {
		ref = name
	}

	chainID, err := catalog.ParseDigest(ref)
	if err != nil {
		return ingest.Request{}, reasonSkip
	}

	diffID, err := catalog.ParseDigest(labels[LabelDiffID])
	if err != nil {
		return ingest.Request{}, reasonSkip
	}

	// An absent label is the signature of disable_snapshot_annotations. A
	// label that is present but unreadable is something else entirely, so it
	// is not worth alarming an operator about the setting they did not change.
	if labels[LabelLayerDigest] == "" {
		return ingest.Request{}, reasonNoAnnotations
	}

	layer, err := digest.Parse(labels[LabelLayerDigest])
	if err != nil {
		return ingest.Request{}, reasonSkip
	}

	return ingest.Request{DiffID: diffID, ChainID: chainID, Layer: layer}, reasonIngest
}

// Provider is the part of containerd's content store ingest reads from. It is
// satisfied by content.Store and by the content service client.
type Provider interface {
	ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error)
}

// ContentOpener streams a layer's uncompressed tar out of containerd's content
// store.
//
// The layer tar is the source of truth for ingest rather than the directory
// this node just unpacked, because that lets a node ingest a layer it never
// unpacked, and because re-tarring a directory would not reproduce the original
// archive byte for byte.
type ContentOpener struct {
	// Provider reads blobs. Required.
	Provider Provider
	// Namespace is the containerd namespace to read in. Empty means
	// DefaultNamespace.
	Namespace string
}

// NewContentOpener returns an opener over the given content store.
func NewContentOpener(p Provider, namespace string) (*ContentOpener, error) {
	if p == nil {
		return nil, fmt.Errorf("snapshotter: content provider is required")
	}

	if namespace == "" {
		namespace = DefaultNamespace
	}

	return &ContentOpener{Provider: p, Namespace: namespace}, nil
}

// Open returns the layer as an uncompressed tar stream.
func (o *ContentOpener) Open(ctx context.Context, req ingest.Request) (ingest.ReadCloser, error) {
	if o.Provider == nil {
		return nil, fmt.Errorf("snapshotter: content provider is required")
	}

	ns := o.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}

	ctx = namespaces.WithNamespace(ctx, ns)

	// ReaderAt only needs the digest; the descriptor's size is filled in from
	// the reader so a caller cannot get it wrong.
	desc := ocispec.Descriptor{Digest: ocidigest.Digest(req.Layer.String())}

	ra, err := o.Provider.ReaderAt(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("open layer %s: %w", req.Layer.String(), err)
	}

	// DecompressStream sniffs the stream, so a gzip, a zstd, and an already
	// uncompressed layer all work without trusting the media type.
	dec, err := compression.DecompressStream(io.NewSectionReader(ra, 0, ra.Size()))
	if err != nil {
		_ = ra.Close() //nolint:errcheck

		return nil, fmt.Errorf("decompress layer %s: %w", req.Layer.String(), err)
	}

	return &layerReader{dec: dec, ra: ra}, nil
}

// layerReader closes the decompressor and the underlying blob together.
type layerReader struct {
	dec compression.DecompressReadCloser
	ra  content.ReaderAt
}

func (r *layerReader) Read(p []byte) (int, error) {
	return r.dec.Read(p)
}

func (r *layerReader) Close() error {
	err := r.dec.Close()

	if cerr := r.ra.Close(); err == nil {
		err = cerr
	}

	return err
}
