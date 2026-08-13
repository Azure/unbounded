// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package snapshotter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"golang.org/x/sync/errgroup"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
)

// mapConcurrency bounds how many layers are mapped and mounted at once. Forty
// layers of an image are independent, so serialising them would add forty
// device-mapper round trips to every first container start; letting all forty
// go at once would burst the device's queue for no gain.
const mapConcurrency = 16

// mountDataLimit is how many bytes of option text mount(2) will accept. The
// kernel copies the option string into a single page and NUL terminates it, so
// anything past that is silently dropped: an overlay whose lowerdir list
// overflows does not fail cleanly, it mounts a short stack and the container
// comes up missing files. containerd checks this as well, but only after it has
// tried to shrink the list, and by then the error names neither the snapshot
// nor the layer count.
var mountDataLimit = os.Getpagesize() - 1

// parentRef is one ancestor of a snapshot, resolved far enough to know where
// its filesystem comes from.
type parentRef struct {
	// ID is the snapshot's numeric identifier, which names its local directory.
	ID string
	// Name is the snapshot's key, which for an image layer is its chain ID.
	Name string
	// DiffID is set when the layer's bytes live in a RACER blob.
	DiffID catalog.Digest
	// Remote reports whether DiffID is set.
	Remote bool
}

// createSnapshot is the ordinary path: a local directory holding the writable
// upper (and, for an active snapshot, the overlay work directory), stacked on
// whatever the parents resolve to.
func (s *Snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		sn      storage.Snapshot
		parents []parentRef
		td      string
		path    string
	)

	defer func() {
		if err == nil {
			return
		}

		if td != "" {
			if rmErr := os.RemoveAll(td); rmErr != nil {
				s.log.Warn("gantry-snapshotter: failed to clean up temp snapshot directory", "path", td, "error", rmErr)
			}
		}

		if path != "" {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				err = fmt.Errorf("failed to remove %s: %v: %w", path, rmErr, err)
			}
		}
	}()

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var txErr error

		td, txErr = s.prepareDirectory(kind)
		if txErr != nil {
			return fmt.Errorf("failed to create prepare snapshot dir: %w", txErr)
		}

		sn, txErr = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if txErr != nil {
			return fmt.Errorf("failed to create snapshot: %w", txErr)
		}

		parents, txErr = s.collectParents(ctx, parent)
		if txErr != nil {
			return txErr
		}

		if len(parents) != len(sn.ParentIDs) {
			return fmt.Errorf("snapshot %q: walked %d parents but storage reports %d: %w",
				key, len(parents), len(sn.ParentIDs), errdefs.ErrFailedPrecondition)
		}

		path = filepath.Join(s.snapshotDir(), sn.ID)
		if txErr = os.Rename(td, path); txErr != nil {
			return fmt.Errorf("failed to rename: %w", txErr)
		}

		td = ""

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Mapping happens outside the transaction: it creates device-mapper targets
	// and mounts filesystems, and holding the metadata write lock across that
	// would serialise every other snapshot operation on the node behind it.
	mounts, err := s.mounts(ctx, sn, parents)
	if err != nil {
		// The snapshot is already committed to the database at this point, so
		// it has to be taken back out. Leaving it would poison the key: the
		// caller's retry would fail with "already exists" against a snapshot
		// that was never usable.
		s.rollback(ctx, key)

		return nil, err
	}

	return mounts, nil
}

// rollback removes a snapshot that was recorded but could not be mounted.
func (s *Snapshotter) rollback(ctx context.Context, key string) {
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, _, err := storage.Remove(ctx, key)

		return err
	})
	if err != nil {
		s.log.Warn("gantry-snapshotter: failed to roll back unmountable snapshot", "key", key, "error", err)
	}
}

// prepareDirectory builds the snapshot directory under a temporary name so a
// failure never leaves a half-built directory under a real snapshot ID.
func (s *Snapshotter) prepareDirectory(kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(s.snapshotDir(), "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0o755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0o711); err != nil {
			return td, err
		}
	}

	return td, nil
}

// collectParents walks the parent chain from name upwards, returning ancestors
// ordered nearest first. That is the same order storage.Snapshot.ParentIDs
// uses, so the two line up index for index.
//
// This must run inside a metastore transaction.
func (s *Snapshotter) collectParents(ctx context.Context, name string) ([]parentRef, error) {
	var out []parentRef

	for name != "" {
		if len(out) >= maxParents {
			return nil, fmt.Errorf("parent chain deeper than %d snapshots: %w", maxParents, errdefs.ErrFailedPrecondition)
		}

		id, snInfo, _, err := storage.GetInfo(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent %q: %w", name, err)
		}

		ref := parentRef{ID: id, Name: name}

		if value := snInfo.Labels[LabelBlob]; value != "" {
			diffID, err := catalog.ParseDigest(value)
			if err != nil {
				return nil, fmt.Errorf("snapshot %q: bad %s label %q: %w", name, LabelBlob, value, err)
			}

			ref.DiffID, ref.Remote = diffID, true
		}

		out = append(out, ref)
		name = snInfo.Parent
	}

	return out, nil
}

// Mounts returns the mounts for an existing active or view snapshot.
func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	var (
		sn      storage.Snapshot
		parents []parentRef
	)

	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var txErr error

		sn, txErr = storage.GetSnapshot(ctx, key)
		if txErr != nil {
			return fmt.Errorf("failed to get snapshot mount: %w", txErr)
		}

		_, snInfo, _, txErr := storage.GetInfo(ctx, key)
		if txErr != nil {
			return txErr
		}

		parents, txErr = s.collectParents(ctx, snInfo.Parent)

		return txErr
	})
	if err != nil {
		return nil, err
	}

	return s.mounts(ctx, sn, parents)
}

// mounts resolves every ancestor to a directory on this node and assembles the
// mount description containerd hands to the runtime.
func (s *Snapshotter) mounts(ctx context.Context, sn storage.Snapshot, parents []parentRef) ([]mount.Mount, error) {
	if len(sn.ParentIDs) == 0 {
		// With no parents overlayfs has nothing to stack, so the writable
		// directory is bind mounted directly.
		roFlag := "rw"
		if sn.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{{
			Source:  s.upperPath(sn.ID),
			Type:    "bind",
			Options: []string{roFlag, "rbind"},
		}}, nil
	}

	paths, err := s.parentPaths(ctx, parents)
	if err != nil {
		return nil, err
	}

	if sn.Kind == snapshots.KindView && len(paths) == 1 {
		// A view of a single layer is that layer, read only. For a RACER backed
		// layer this binds the EROFS mount, which is already read only.
		return []mount.Mount{{
			Source:  paths[0],
			Type:    "bind",
			Options: []string{"ro", "rbind"},
		}}, nil
	}

	var options []string

	if sn.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", s.workPath(sn.ID)),
			fmt.Sprintf("upperdir=%s", s.upperPath(sn.ID)),
		)
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(paths, ":")))
	options = append(options, s.opts...)

	if err := s.checkOptions(options, len(paths)); err != nil {
		return nil, err
	}

	return []mount.Mount{{
		Type:    "overlay",
		Source:  "overlay",
		Options: options,
	}}, nil
}

// checkOptions refuses an overlay the kernel would truncate.
//
// containerd shortens a long lowerdir list before it calls mount(2): it finds
// the directory every lower shares, chdirs there, and passes the rest relative
// (compactLowerdirOption in core/mount). That saves the shared prefix from
// every lower, so model the same saving here rather than refusing a mount that
// would in fact have gone through.
//
// The saving modelled is the one this snapshotter can guarantee, the parent
// its own root and the mapper's root have in common. containerd computes the
// prefix from the actual paths, which is never shorter, so a stack this accepts
// is always one containerd can fit. A stack this refuses could in principle
// have fit, but only past roughly a hundred and forty layers, well beyond what
// any image format allows.
func (s *Snapshotter) checkOptions(options []string, layers int) error {
	size := len(options) - 1
	for _, o := range options {
		size += len(o)
	}

	if s.lowerRoot != "" && layers > 1 {
		size -= layers * (len(s.lowerRoot) + 1)
	}

	if size <= mountDataLimit {
		return nil
	}

	return fmt.Errorf("overlay of %d layers needs %d bytes of mount options but the kernel takes %d: %w",
		layers, size, mountDataLimit, errdefs.ErrFailedPrecondition)
}

// commonRoot is the deepest directory both paths are under, or "" when they
// share nothing but the filesystem root.
func commonRoot(a, b string) string {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return ""
	}

	as, bs := strings.Split(a, "/"), strings.Split(b, "/")

	var shared []string

	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			break
		}

		shared = append(shared, as[i])
	}

	// The split of an absolute path starts with an empty element, so a join of
	// nothing but that is "", which is the case where the two share only "/".
	return strings.Join(shared, "/")
}

// parentPaths turns ancestors into directories, mapping and mounting any RACER
// backed layer that is not already present on the node.
func (s *Snapshotter) parentPaths(ctx context.Context, parents []parentRef) ([]string, error) {
	paths := make([]string, len(parents))

	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(mapConcurrency)

	for i, p := range parents {
		if !p.Remote {
			paths[i] = s.upperPath(p.ID)
			continue
		}

		group.Go(func() error {
			blob, ok := s.blob(p.DiffID)
			if !ok {
				return fmt.Errorf("snapshot %q: layer %s is not in the catalog: %w",
					p.Name, p.DiffID.Short(), errdefs.ErrNotFound)
			}

			path, err := s.maps.Ensure(gctx, p.DiffID, blob.Address)
			if err != nil {
				return fmt.Errorf("snapshot %q: map layer %s: %w", p.Name, p.DiffID.Short(), err)
			}

			paths[i] = path

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return paths, nil
}

// defaultMountOptions picks the overlay options this kernel wants. index=on
// keeps an inode index that only pays off for hardlink-heavy upper directories
// and that the built in overlay snapshotter also turns off.
func defaultMountOptions() []string {
	if _, err := os.Stat("/sys/module/overlay/parameters/index"); err != nil {
		return nil
	}

	return []string{"index=off"}
}
