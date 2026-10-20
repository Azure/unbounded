// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	overlaysnapshotter "github.com/containerd/containerd/v2/plugins/snapshots/overlay"
	"github.com/containerd/errdefs"
	"google.golang.org/grpc/metadata"
)

func TestSnapshotterCleanupEmptyStore(t *testing.T) {
	snapshotter, err := NewSnapshotter(t.TempDir(), &recordingMounter{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	if err := snapshotter.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotterRemotePullAndRuntimeLifecycle(t *testing.T) {
	ctx := context.Background()
	mounter := &recordingMounter{}

	snapshotter, err := NewSnapshotter(t.TempDir(), mounter)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	prepareRemote(t, ctx, snapshotter, "extract-1", "layer-1", "", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "4096")
	prepareRemote(t, ctx, snapshotter, "extract-2", "layer-2", "layer-1", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "8192")

	info, err := snapshotter.Stat(ctx, "layer-2")
	if err != nil {
		t.Fatal(err)
	}

	if info.Kind != snapshots.KindCommitted || info.Parent != "layer-1" || info.Labels[remoteSnapshotLabel] != remoteSnapshotValue {
		t.Fatalf("remote info = %+v", info)
	}

	mounts, err := snapshotter.Prepare(ctx, "container", "layer-2")
	if err != nil {
		t.Fatal(err)
	}

	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		t.Fatalf("runtime mounts = %+v", mounts)
	}

	if len(mounter.calls) != 1 || len(mounter.calls[0].layers) != 2 {
		t.Fatalf("mounter calls = %+v", mounter.calls)
	}

	if got := mounter.calls[0].layers; got[0].Digest.String()[7] != 'a' || got[1].Digest.String()[7] != 'b' {
		t.Fatalf("layer order = %+v", got)
	}

	if _, err := snapshotter.Mounts(ctx, "container"); err != nil {
		t.Fatal(err)
	}

	if len(mounter.calls) != 1 {
		t.Fatalf("Mounts attached image %d times", len(mounter.calls))
	}

	if err := snapshotter.Remove(ctx, "container"); err != nil {
		t.Fatal(err)
	}

	if !mounter.calls[0].closed {
		t.Fatal("runtime block mount was not closed")
	}
}

func TestSnapshotterRestartPreservesLiveRemoteSnapshots(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	first, err := NewSnapshotter(root, &recordingMounter{})
	if err != nil {
		t.Fatal(err)
	}

	prepareRemote(t, ctx, first, "extract", "layer", "", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "4096")

	if _, err := first.Prepare(ctx, "container", "layer"); err != nil {
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	mounter := &recordingMounter{}

	second, err := NewSnapshotter(root, mounter)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = second.Close() })

	if err := second.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := second.Stat(ctx, "layer"); err != nil {
		t.Fatalf("remote layer lost after restart: %v", err)
	}

	if _, err := second.Mounts(ctx, "container"); err != nil {
		t.Fatalf("runtime snapshot lost after restart: %v", err)
	}

	if len(mounter.calls) != 1 {
		t.Fatalf("mounter calls after restart = %d, want 1", len(mounter.calls))
	}
}

func TestSnapshotterOrdinaryLayerFallback(t *testing.T) {
	ctx := context.Background()

	snapshotter, err := NewSnapshotter(t.TempDir(), &recordingMounter{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	mounts, err := snapshotter.Prepare(ctx, "extract", "", snapshots.WithLabels(map[string]string{
		targetSnapshotRefLabel: "layer",
		targetImageRefLabel:    "mcr.microsoft.com/oss/v2/kubernetes/pause:3.10.2",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if len(mounts) == 0 {
		t.Fatal("ordinary layer prepare returned no writable mount")
	}

	if err := snapshotter.Commit(ctx, "layer", "extract"); err != nil {
		t.Fatal(err)
	}

	info, err := snapshotter.Stat(ctx, "layer")
	if err != nil {
		t.Fatal(err)
	}

	if info.Kind != snapshots.KindCommitted || info.Labels[remoteSnapshotLabel] != "" {
		t.Fatalf("ordinary layer info = %+v", info)
	}

	if _, err := snapshotter.Prepare(ctx, "container", "layer"); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotterInjectedFallback(t *testing.T) {
	ctx := context.Background()

	fallback, err := overlaysnapshotter.NewSnapshotter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	snapshotter, err := NewSnapshotterWithFallback(t.TempDir(), &recordingMounter{}, fallback)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	if _, err := snapshotter.Prepare(ctx, "extract", ""); err != nil {
		t.Fatal(err)
	}

	if err := snapshotter.Commit(ctx, "layer", "extract"); err != nil {
		t.Fatal(err)
	}

	if _, err := fallback.Stat(ctx, "layer"); err != nil {
		t.Fatalf("injected fallback did not receive layer: %v", err)
	}
}

func TestForwardNamespace(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(namespaces.GRPCHeader, "k8s.io"))
	forwarded := forwardNamespace(ctx)

	outgoing, ok := metadata.FromOutgoingContext(forwarded)
	if !ok || len(outgoing.Get(namespaces.GRPCHeader)) != 1 || outgoing.Get(namespaces.GRPCHeader)[0] != "k8s.io" {
		t.Fatalf("outgoing namespace metadata = %v", outgoing)
	}
}

func TestSnapshotterRejectsPartialRemoteContract(t *testing.T) {
	ctx := context.Background()

	snapshotter, err := NewSnapshotter(t.TempDir(), &recordingMounter{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	_, err = snapshotter.Prepare(ctx, "extract", "", snapshots.WithLabels(map[string]string{
		targetSnapshotRefLabel: "layer",
		targetImageRefLabel:    "example.azurecr.io/repository:tag",
		AnnotationBlobDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}))
	if err == nil || errdefs.IsAlreadyExists(err) {
		t.Fatalf("partial contract error = %v", err)
	}

	if _, err := snapshotter.Stat(ctx, "layer"); !errdefs.IsNotFound(err) {
		t.Fatalf("partial contract snapshot exists: %v", err)
	}
}

func TestSnapshotterRetainsMetadataWhenUnmountFails(t *testing.T) {
	ctx := context.Background()
	mounter := &recordingMounter{}

	snapshotter, err := NewSnapshotter(t.TempDir(), mounter)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = snapshotter.Close() })

	prepareRemote(t, ctx, snapshotter, "extract", "layer", "", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "4096")

	if _, err := snapshotter.Prepare(ctx, "container", "layer"); err != nil {
		t.Fatal(err)
	}

	mounter.calls[0].closeErr = errors.New("busy")

	if err := snapshotter.Remove(ctx, "container"); err == nil {
		t.Fatal("Remove succeeded after unmount failure")
	}

	if _, err := snapshotter.Stat(ctx, "container"); err != nil {
		t.Fatalf("active metadata removed after unmount failure: %v", err)
	}

	if err := snapshotter.Remove(ctx, "container"); err != nil {
		t.Fatalf("retry Remove: %v", err)
	}
}

func TestNewNBDMounterAllowsDevicesToAppearLater(t *testing.T) {
	if _, err := NewNBDMounter("http://127.0.0.1:5000/blobs", nil, filepath.Join(t.TempDir(), "nbd*")); err != nil {
		t.Fatal(err)
	}
}

func TestNewNBDMounterRejectsInvalidEndpoint(t *testing.T) {
	if _, err := NewNBDMounter("https://gantry.example.com/blobs", nil, "/dev/nbd*"); err == nil {
		t.Fatal("NewNBDMounter accepted a non-loopback HTTPS endpoint")
	}
}

func TestBlobOriginURL(t *testing.T) {
	got, err := blobOriginURL("example.azurecr.io/team/image:tag", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}

	if want := "https://example.azurecr.io/v2/team/image/blobs/sha256:abc"; got != want {
		t.Fatalf("blob URL = %q, want %q", got, want)
	}
}

func TestParseFilesystemSpec(t *testing.T) {
	filesystem, options, err := parseFilesystemSpec("ext4;noload,discard;ignored-mkfs-options")
	if err != nil {
		t.Fatal(err)
	}

	if filesystem != "ext4" || len(options) != 2 || options[0] != "noload" || options[1] != "discard" {
		t.Fatalf("filesystem spec = %q, %v", filesystem, options)
	}

	if _, _, err := parseFilesystemSpec("xfs"); err == nil {
		t.Fatal("unsupported filesystem accepted")
	}
}

func prepareRemote(t *testing.T, ctx context.Context, snapshotter *Snapshotter, key, target, parent, blobDigest, blobSize string) {
	t.Helper()

	_, err := snapshotter.Prepare(ctx, key, parent, snapshots.WithLabels(map[string]string{
		targetSnapshotRefLabel: "" + target,
		targetImageRefLabel:    "example.azurecr.io/team/image:tag",
		AnnotationBlobDigest:   blobDigest,
		AnnotationBlobSize:     blobSize,
		AnnotationBlobFSType:   "ext4",
		AnnotationVersion:      VersionV1,
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("remote prepare error = %v", err)
	}
}

type recordingMount struct {
	layers   []RemoteLayer
	path     string
	closed   bool
	closeErr error
}

type recordingMounter struct {
	calls []*recordingMount
}

func (m *recordingMounter) Mount(_ context.Context, _ string, layers []RemoteLayer, path string) (io.Closer, error) {
	if len(layers) == 0 {
		return nil, errors.New("no layers")
	}

	mounted := &recordingMount{layers: append([]RemoteLayer(nil), layers...), path: filepath.Clean(path)}
	m.calls = append(m.calls, mounted)

	return mounted, nil
}

func (m *recordingMount) Close() error {
	if m.closeErr != nil {
		err := m.closeErr
		m.closeErr = nil

		return err
	}

	m.closed = true

	return nil
}
