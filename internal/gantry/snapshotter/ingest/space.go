// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"errors"
	"fmt"
)

// DefaultHeadroom is the free space on the work filesystem that an ingest
// refuses to spend.
//
// The work directory is not this process's private scratch space. It is a
// hostPath on the node's root filesystem, which it shares with every other
// pod's writable layer, with containerd's content store, and with the kubelet's
// own eviction thresholds. Filling it does not fail an ingest, it evicts the
// node's workloads, and ingest is the one thing here that is nowhere near the
// container start path and can always wait.
const DefaultHeadroom = 4 << 30

// spillShare is how many copies of a layer the work filesystem has to hold at
// once. mkfs.erofs writes its image beside the tarball rather than consuming it
// in place, so both exist until the build finishes.
const spillShare = 2

// ErrNoSpace reports that the work filesystem cannot hold this ingest without
// eating into the reserve.
//
// It is not a permanent failure. The layer is already unpacked locally and the
// container it belongs to is already running; some other node will ingest it,
// or this one will once its disk frees up.
var ErrNoSpace = errors.New("ingest: not enough room on the work filesystem")

// FreeFunc reports the bytes available on the filesystem holding path.
type FreeFunc func(path string) (uint64, error)

// spillLimit is the largest layer tarball this node can convert right now.
func (i *Ingester) spillLimit() (uint64, error) {
	avail, err := i.free(i.workDir)
	if err != nil {
		return 0, fmt.Errorf("ingest: work filesystem: %w", err)
	}

	if avail <= i.headroom {
		return 0, fmt.Errorf("%w: %d bytes free, %d reserved", ErrNoSpace, avail, i.headroom)
	}

	return (avail - i.headroom) / spillShare, nil
}

// roomFor reports whether the work filesystem can still spare need bytes on top
// of the reserve.
//
// The tarball is already on disk by the time this runs, so this is the check
// that the image can follow it. The limit spillLimit imposed was computed
// before the tarball landed and against a number that other pods on the node
// are free to move underneath us.
func (i *Ingester) roomFor(need uint64) error {
	avail, err := i.free(i.workDir)
	if err != nil {
		return fmt.Errorf("ingest: work filesystem: %w", err)
	}

	if avail < need+i.headroom {
		return fmt.Errorf("%w: %d bytes free, %d needed on top of %d reserved",
			ErrNoSpace, avail, need, i.headroom)
	}

	return nil
}
