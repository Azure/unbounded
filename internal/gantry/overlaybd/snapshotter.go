// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	overlaysnapshotter "github.com/containerd/containerd/v2/plugins/snapshots/overlay"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
)

const (
	targetSnapshotRefLabel = "containerd.io/snapshot.ref"
	targetImageRefLabel    = "containerd.io/snapshot/image-ref"
	criImageRefLabel       = "containerd.io/snapshot/cri.image-ref"
	remoteSnapshotLabel    = "containerd.io/snapshot/remote"
	remoteSnapshotValue    = "remote snapshot"
)

// RemoteLayer describes one ACR-produced OverlayBD layer in manifest order.
type RemoteLayer struct {
	Digest   digest.Digest
	Size     int64
	ImageRef string
	FSType   string
}

// BlockMounter exposes a bottom-to-top remote layer chain as one mounted
// read-only filesystem. Closing the result tears down the mount and device.
type BlockMounter interface {
	Mount(context.Context, string, []RemoteLayer, string) (io.Closer, error)
}

// Snapshotter implements the containerd remote snapshot protocol for the
// existing ACR OverlayBD v0.1.0 contract.
type Snapshotter struct {
	root         string
	store        *storage.MetaStore
	local        snapshots.Snapshotter
	cleanupLocal bool
	mounter      BlockMounter

	mountMu  sync.Mutex
	mounts   map[string]io.Closer
	close    sync.Once
	closeErr error
}

func NewSnapshotter(root string, mounter BlockMounter) (*Snapshotter, error) {
	return newSnapshotter(root, mounter, nil, true)
}

func NewSnapshotterWithFallback(root string, mounter BlockMounter, fallback snapshots.Snapshotter) (*Snapshotter, error) {
	if fallback == nil {
		return nil, errors.New("overlaybd snapshotter: fallback snapshotter is required")
	}

	return newSnapshotter(root, mounter, fallback, false)
}

func newSnapshotter(root string, mounter BlockMounter, local snapshots.Snapshotter, cleanupLocal bool) (*Snapshotter, error) {
	if root == "" || mounter == nil {
		return nil, errors.New("overlaybd snapshotter: root and block mounter are required")
	}

	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0o700); err != nil {
		return nil, fmt.Errorf("overlaybd snapshotter: create root: %w", err)
	}

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("overlaybd snapshotter: resolve root: %w", err)
	}

	store, err := storage.NewMetaStore(filepath.Join(resolvedRoot, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("overlaybd snapshotter: metadata: %w", err)
	}

	if local == nil {
		local, err = overlaysnapshotter.NewSnapshotter(filepath.Join(resolvedRoot, "local"))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("overlaybd snapshotter: local fallback: %w", err), store.Close())
		}
	}

	return &Snapshotter{
		root:         resolvedRoot,
		store:        store,
		local:        local,
		cleanupLocal: cleanupLocal,
		mounter:      mounter,
		mounts:       make(map[string]io.Closer),
	}, nil
}

func (s *Snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if errdefs.IsNotFound(err) {
		return s.local.Stat(forwardNamespace(ctx), key)
	}

	return info, err
}

func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (updated snapshots.Info, err error) {
	remote, err := s.hasRemoteSnapshot(ctx, info.Name)
	if err != nil {
		return snapshots.Info{}, err
	}

	if !remote {
		return s.local.Update(forwardNamespace(ctx), info, fieldpaths...)
	}

	err = s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
		updated, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})

	return updated, err
}

func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	remote, err := s.hasRemoteSnapshot(ctx, key)
	if err != nil {
		return snapshots.Usage{}, err
	}

	if !remote {
		return s.local.Usage(forwardNamespace(ctx), key)
	}

	var (
		id    string
		info  snapshots.Info
		usage snapshots.Usage
	)

	if err = s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return snapshots.Usage{}, err
	}

	if info.Kind != snapshots.KindActive {
		return usage, nil
	}

	diskUsage, err := fs.DiskUsage(ctx, s.upperPath(id))
	if err != nil {
		return snapshots.Usage{}, err
	}

	return snapshots.Usage(diskUsage), nil
}

func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.create(ctx, snapshots.KindActive, key, parent, opts...)
}

func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.create(ctx, snapshots.KindView, key, parent, opts...)
}

func (s *Snapshotter) create(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	var requested snapshots.Info
	for _, opt := range opts {
		if err := opt(&requested); err != nil {
			return nil, err
		}
	}

	if target := requested.Labels[targetSnapshotRefLabel]; target != "" {
		if !hasRemoteLayerAnnotations(requested.Labels) {
			return s.createLocal(ctx, kind, key, parent, opts...)
		}

		if kind != snapshots.KindActive {
			return nil, fmt.Errorf("overlaybd snapshotter: remote target requires active prepare: %w", errdefs.ErrInvalidArgument)
		}

		if _, err := remoteLayerFromLabels(requested.Labels); err != nil {
			return nil, err
		}

		requested.Labels[remoteSnapshotLabel] = remoteSnapshotValue
		if err := s.commitRemote(ctx, key, target, parent, requested.Labels); err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("target snapshot %q: %w", target, errdefs.ErrAlreadyExists)
	}

	remoteParent, err := s.hasRemoteSnapshot(ctx, parent)
	if err != nil {
		return nil, err
	}

	if !remoteParent {
		return s.createLocal(ctx, kind, key, parent, opts...)
	}

	var snapshot storage.Snapshot

	err = s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error

		snapshot, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("overlaybd snapshotter: create snapshot: %w", err)
	}

	if err := s.prepareSnapshotDirectory(snapshot.ID, kind); err != nil {
		return nil, errors.Join(err, s.removeMetadata(ctx, key), os.RemoveAll(s.snapshotPath(snapshot.ID)))
	}

	if err := s.ensureMounted(ctx, snapshot.ID, key); err != nil {
		return nil, errors.Join(err, s.removeMetadata(ctx, key), os.RemoveAll(s.snapshotPath(snapshot.ID)))
	}

	return s.runtimeMounts(snapshot), nil
}

func (s *Snapshotter) createLocal(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	ctx = forwardNamespace(ctx)
	if kind == snapshots.KindView {
		return s.local.View(ctx, key, parent, opts...)
	}

	return s.local.Prepare(ctx, key, parent, opts...)
}

func (s *Snapshotter) commitRemote(ctx context.Context, key, target, parent string, labels map[string]string) error {
	var id string

	err := s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
		snapshot, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, snapshots.WithLabels(labels))
		if err != nil {
			return err
		}

		id = snapshot.ID
		_, err = storage.CommitActive(ctx, key, target, snapshots.Usage{}, snapshots.WithLabels(labels))

		return err
	})
	if err != nil {
		return fmt.Errorf("overlaybd snapshotter: commit remote snapshot: %w", err)
	}

	if err := os.MkdirAll(s.snapshotPath(id), 0o700); err != nil {
		return errors.Join(fmt.Errorf("overlaybd snapshotter: create remote directory: %w", err), s.removeMetadata(ctx, target))
	}

	return nil
}

func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	remote, err := s.hasRemoteSnapshot(ctx, key)
	if err != nil {
		return nil, err
	}

	if !remote {
		return s.local.Mounts(forwardNamespace(ctx), key)
	}

	var snapshot storage.Snapshot

	if err = s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		snapshot, err = storage.GetSnapshot(ctx, key)
		return err
	}); err != nil {
		return nil, err
	}

	if err := s.ensureMounted(ctx, snapshot.ID, key); err != nil {
		return nil, err
	}

	return s.runtimeMounts(snapshot), nil
}

func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	remote, err := s.hasRemoteSnapshot(ctx, key)
	if err != nil {
		return err
	}

	if remote {
		return fmt.Errorf("overlaybd snapshotter: writable remote snapshot commit: %w", errdefs.ErrNotImplemented)
	}

	return s.local.Commit(forwardNamespace(ctx), name, key, opts...)
}

func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	remote, err := s.hasRemoteSnapshot(ctx, key)
	if err != nil {
		return err
	}

	if !remote {
		return s.local.Remove(forwardNamespace(ctx), key)
	}

	var id string

	if err := s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error

		id, _, _, err = storage.GetInfo(ctx, key)

		return err
	}); err != nil {
		return fmt.Errorf("overlaybd snapshotter: inspect %s for removal: %w", key, err)
	}

	closed, err := s.closeMount(id)
	if err != nil {
		return fmt.Errorf("overlaybd snapshotter: unmount %s: %w", key, err)
	}

	if !closed {
		if err := mount.UnmountAll(s.lowerPath(id), 0); err != nil {
			return fmt.Errorf("overlaybd snapshotter: clear stale mount %s: %w", key, err)
		}
	}

	if err := s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, _, err := storage.Remove(ctx, key)
		return err
	}); err != nil {
		return fmt.Errorf("overlaybd snapshotter: remove %s: %w", key, err)
	}

	if err := os.RemoveAll(s.snapshotPath(id)); err != nil {
		return fmt.Errorf("overlaybd snapshotter: remove directory: %w", err)
	}

	return nil
}

func (s *Snapshotter) Walk(ctx context.Context, walk snapshots.WalkFunc, filters ...string) error {
	if err := s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, walk, filters...)
	}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}

	return s.local.Walk(forwardNamespace(ctx), walk, filters...)
}

func (s *Snapshotter) Cleanup(ctx context.Context) error {
	var localErr error
	if cleaner, ok := s.local.(snapshots.Cleaner); s.cleanupLocal && ok {
		localErr = cleaner.Cleanup(forwardNamespace(ctx))
		if errdefs.IsNotFound(localErr) {
			localErr = nil
		}
	}

	return errors.Join(s.cleanupRemote(ctx), localErr)
}

func (s *Snapshotter) cleanupRemote(ctx context.Context) error {
	var live map[string]struct{}

	if err := s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		ids, err := storage.IDMap(ctx)
		if err != nil {
			if errdefs.IsNotFound(err) {
				live = map[string]struct{}{}
				return nil
			}

			return err
		}

		live = make(map[string]struct{}, len(ids))
		for id := range ids {
			live[id] = struct{}{}
		}

		return nil
	}); err != nil {
		return err
	}

	entries, err := os.ReadDir(filepath.Join(s.root, "snapshots"))
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if _, ok := live[entry.Name()]; ok {
			continue
		}

		closed, err := s.closeMount(entry.Name())
		if err != nil {
			return fmt.Errorf("overlaybd snapshotter: cleanup mount %s: %w", entry.Name(), err)
		}

		if !closed {
			if err := mount.UnmountAll(s.lowerPath(entry.Name()), 0); err != nil {
				return fmt.Errorf("overlaybd snapshotter: cleanup stale mount %s: %w", entry.Name(), err)
			}
		}

		if err := os.RemoveAll(s.snapshotPath(entry.Name())); err != nil {
			return err
		}
	}

	return nil
}

func (s *Snapshotter) Close() error {
	s.close.Do(func() {
		s.mountMu.Lock()
		mounts := s.mounts
		s.mounts = make(map[string]io.Closer)
		s.mountMu.Unlock()

		var errs []error

		for _, mounted := range mounts {
			if err := mounted.Close(); err != nil {
				errs = append(errs, err)
			}
		}

		if err := s.store.Close(); err != nil {
			errs = append(errs, err)
		}

		if err := s.local.Close(); err != nil {
			errs = append(errs, err)
		}

		s.closeErr = errors.Join(errs...)
	})

	return s.closeErr
}

func (s *Snapshotter) hasRemoteSnapshot(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}

	err := s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, _, _, err := storage.GetInfo(ctx, key)
		return err
	})
	if err == nil {
		return true, nil
	}

	if errdefs.IsNotFound(err) {
		return false, nil
	}

	return false, err
}

func hasRemoteLayerAnnotations(labels map[string]string) bool {
	return labels[AnnotationBlobDigest] != "" ||
		labels[AnnotationBlobSize] != "" ||
		labels[AnnotationBlobFSType] != "" ||
		labels[AnnotationVersion] != ""
}

func forwardNamespace(ctx context.Context) context.Context {
	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		return ctx
	}

	return namespaces.WithNamespace(ctx, namespace)
}

func (s *Snapshotter) ensureMounted(ctx context.Context, id, key string) error {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()

	if _, ok := s.mounts[id]; ok {
		return nil
	}

	layers, err := s.remoteLayers(ctx, key)
	if err != nil {
		return err
	}

	mounted, err := s.mounter.Mount(ctx, id, layers, s.lowerPath(id))
	if err != nil {
		return fmt.Errorf("overlaybd snapshotter: mount image: %w", err)
	}

	s.mounts[id] = mounted

	return nil
}

func (s *Snapshotter) remoteLayers(ctx context.Context, key string) ([]RemoteLayer, error) {
	var layers []RemoteLayer

	err := s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}

		for parent := info.Parent; parent != ""; {
			_, parentInfo, _, err := storage.GetInfo(ctx, parent)
			if err != nil {
				return err
			}

			layer, err := remoteLayerFromLabels(parentInfo.Labels)
			if err != nil {
				return fmt.Errorf("overlaybd snapshotter: parent %s: %w", parent, err)
			}

			layers = append(layers, layer)
			parent = parentInfo.Parent
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(layers) == 0 {
		return nil, fmt.Errorf("overlaybd snapshotter: no remote layers: %w", errdefs.ErrInvalidArgument)
	}

	for left, right := 0, len(layers)-1; left < right; left, right = left+1, right-1 {
		layers[left], layers[right] = layers[right], layers[left]
	}

	return layers, nil
}

func remoteLayerFromLabels(labels map[string]string) (RemoteLayer, error) {
	dgst, err := digest.Parse(labels[AnnotationBlobDigest])
	if err != nil {
		return RemoteLayer{}, fmt.Errorf("overlaybd snapshotter: invalid blob digest: %w", err)
	}

	size, err := strconv.ParseInt(labels[AnnotationBlobSize], 10, 64)
	if err != nil || size <= 0 {
		return RemoteLayer{}, fmt.Errorf("overlaybd snapshotter: invalid blob size %q", labels[AnnotationBlobSize])
	}

	version := labels[AnnotationVersion]
	if version != "" && version != VersionV1 {
		return RemoteLayer{}, fmt.Errorf("overlaybd snapshotter: unsupported version %q", version)
	}

	imageRef := labels[targetImageRefLabel]
	if imageRef == "" {
		imageRef = labels[criImageRefLabel]
	}

	if imageRef == "" {
		return RemoteLayer{}, errors.New("overlaybd snapshotter: image reference label is required")
	}

	return RemoteLayer{
		Digest:   dgst,
		Size:     size,
		ImageRef: imageRef,
		FSType:   labels[AnnotationBlobFSType],
	}, nil
}

func (s *Snapshotter) runtimeMounts(snapshot storage.Snapshot) []mount.Mount {
	if snapshot.Kind == snapshots.KindView {
		return []mount.Mount{{Source: s.lowerPath(snapshot.ID), Type: "bind", Options: []string{"ro", "rbind"}}}
	}

	return []mount.Mount{{
		Source: "overlay",
		Type:   "overlay",
		Options: []string{
			"index=off",
			fmt.Sprintf("lowerdir=%s", s.lowerPath(snapshot.ID)),
			fmt.Sprintf("upperdir=%s", s.upperPath(snapshot.ID)),
			fmt.Sprintf("workdir=%s", s.workPath(snapshot.ID)),
		},
	}}
}

func (s *Snapshotter) prepareSnapshotDirectory(id string, kind snapshots.Kind) error {
	if err := os.MkdirAll(s.lowerPath(id), 0o700); err != nil {
		return fmt.Errorf("overlaybd snapshotter: create lower directory: %w", err)
	}

	if kind == snapshots.KindActive {
		if err := os.MkdirAll(s.upperPath(id), 0o755); err != nil {
			return fmt.Errorf("overlaybd snapshotter: create upper directory: %w", err)
		}

		if err := os.MkdirAll(s.workPath(id), 0o711); err != nil {
			return fmt.Errorf("overlaybd snapshotter: create work directory: %w", err)
		}
	}

	return nil
}

func (s *Snapshotter) removeMetadata(ctx context.Context, key string) error {
	return s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, _, err := storage.Remove(ctx, key)
		return err
	})
}

func (s *Snapshotter) closeMount(id string) (bool, error) {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()

	mounted := s.mounts[id]
	if mounted == nil {
		return false, nil
	}

	if err := mounted.Close(); err != nil {
		return true, err
	}

	delete(s.mounts, id)

	return true, nil
}

func (s *Snapshotter) snapshotPath(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

func (s *Snapshotter) lowerPath(id string) string {
	return filepath.Join(s.snapshotPath(id), "lower")
}

func (s *Snapshotter) upperPath(id string) string {
	return filepath.Join(s.snapshotPath(id), "fs")
}

func (s *Snapshotter) workPath(id string) string {
	return filepath.Join(s.snapshotPath(id), "work")
}
