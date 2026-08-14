// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package blockmap

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// erofsSuperMagic identifies an EROFS mount. Asking statfs what filesystem is
// at a path is how this package tells a mounted layer from an empty directory:
// it is one syscall, it needs no parsing of /proc/self/mountinfo, and it cannot
// be confused by another filesystem mounted at the same place.
const erofsSuperMagic = 0xe0f5e1e2

// FSType is the filesystem layers are stored as.
//
// EROFS is chosen because it is read-only by construction, its on-disk layout
// is a directly mappable image rather than a stream, and the kernel reads it
// with ordinary block I/O. That last point is what makes it fit RACER: a page
// fault on a file in a layer becomes a read of a 4 KiB range of an immutable
// page, which is exactly the access RACER serves without a confirmation round.
const FSType = "erofs"

// SystemMounter mounts through the kernel directly. There is no mount(8) fork
// on this path: a container start can involve tens of layer mounts and each
// fork would be milliseconds of pure overhead.
type SystemMounter struct{}

// NewSystemMounter returns a Mounter that calls the kernel.
func NewSystemMounter() SystemMounter { return SystemMounter{} }

// Mount implements Mounter.
//
// The flags are the least privilege a layer needs. A layer is read-only data
// and nothing in it should ever be executable as a device node or gain
// privileges, and a compromised image should not be able to change that by
// what it contains.
func (SystemMounter) Mount(_ context.Context, source, target string) error {
	const flags = unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV

	if err := unix.Mount(source, target, FSType, flags, ""); err != nil {
		return fmt.Errorf("mount %s at %s as %s: %w", source, target, FSType, err)
	}

	return nil
}

// Unmount implements Mounter. Unmounting something that is not mounted is not
// an error, because Prune runs against state it did not necessarily create.
func (m SystemMounter) Unmount(_ context.Context, target string) error {
	err := unix.Unmount(target, unix.UMOUNT_NOFOLLOW)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
		return nil
	default:
		return fmt.Errorf("unmount %s: %w", target, err)
	}
}

// Mounted implements Mounter.
func (SystemMounter) Mounted(target string) (bool, error) {
	var st unix.Statfs_t

	if err := unix.Statfs(target, &st); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}

		return false, fmt.Errorf("statfs %s: %w", target, err)
	}

	return st.Type == erofsSuperMagic, nil
}
