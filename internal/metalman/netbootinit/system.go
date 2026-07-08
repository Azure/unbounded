// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type SystemOps interface {
	KernelRelease() (string, error)
	Mount(source, target, fstype string) error
	Unmount(target string) error
	RereadPartitionTable(path string) error
	Reboot() error
	Sync()
}

type realSystemOps struct{}

func (realSystemOps) KernelRelease() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", err
	}

	return bytesToString(uts.Release[:]), nil
}

func (realSystemOps) Mount(source, target, fstype string) error {
	if err := unix.Mount(source, target, fstype, 0, ""); err != nil {
		return err
	}

	return nil
}

func (realSystemOps) Unmount(target string) error {
	return unix.Unmount(target, 0)
}

func (realSystemOps) RereadPartitionTable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer closeBestEffort(f)

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.BLKRRPART), 0); errno != 0 {
		return errno
	}

	return nil
}

func (realSystemOps) Reboot() error {
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}

func (realSystemOps) Sync() { unix.Sync() }

func bytesToString(chars []byte) string {
	for idx, char := range chars {
		if char == 0 {
			return string(chars[:idx])
		}
	}

	return string(chars)
}

func (i *Installer) mountAny(source, target string, filesystems ...string) error {
	var lastErr error

	for _, fstype := range filesystems {
		if err := i.System.Mount(source, target, fstype); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}

	if lastErr == nil {
		return errors.New("no filesystems to try")
	}

	return lastErr
}

func (i *Installer) withMounted(source, target string, filesystems []string, fn func() error) error {
	if err := i.mountAny(source, target, filesystems...); err != nil {
		return err
	}

	mounted := true
	defer func() {
		if mounted {
			unmountBestEffort(i.System, target)
		}
	}()

	if err := fn(); err != nil {
		return err
	}

	if err := i.System.Unmount(target); err != nil {
		return err
	}

	mounted = false

	return nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
