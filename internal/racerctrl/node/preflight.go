// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// Host prerequisite paths. These are the things R9 says must be true on a node
// before racer can serve, and none of them are things racer can arrange for
// itself.
const (
	ublkControlPath = "/dev/ublk-control"
	ublksMaxPath    = "/sys/module/ublk_drv/parameters/ublks_max"
)

// directProbeSize is one 4 KiB block, the smallest unit racer writes. The probe
// buffer is twice that so it can be sliced to a 4 KiB aligned interior, which
// O_DIRECT requires of both the buffer address and the length.
const directProbeSize = racerctrl.SmallPage

// Preflight verifies the host prerequisites racer needs and returns an error
// naming every unmet one. It is deliberately a separate entry point from Run so
// the DaemonSet can wire it as an init container: a node that cannot satisfy
// these should fail loudly at startup rather than have racer crash-loop with a
// less legible error.
//
// The checks are read-only except for a single temporary file under the store
// directory, which is removed before returning.
func Preflight(cfg Config) error {
	var problems []string

	if err := checkUblkControl(); err != nil {
		problems = append(problems, err.Error())
	}

	if err := checkUblksMax(); err != nil {
		problems = append(problems, err.Error())
	}

	if err := checkStore(cfg.StorePath); err != nil {
		problems = append(problems, err.Error())
	}

	if cfg.FabricEnabled() {
		if err := checkNvmet(cfg.NvmetRoot); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("host is not ready to run racer:\n  - %s", strings.Join(problems, "\n  - "))
}

// checkUblkControl confirms the ublk control device exists and can be opened.
// Opening it is the only way to tell an unloaded module from one whose device
// node is present but unusable.
func checkUblkControl() error {
	f, err := os.OpenFile(ublkControlPath, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is missing: load the ublk_drv module (modprobe ublk_drv)", ublkControlPath)
		}

		return fmt.Errorf("open %s: %w", ublkControlPath, err)
	}

	return f.Close()
}

// checkUblksMax confirms the driver will admit as many devices as racer's
// runtime budget allows. The module default is 64; racer can export up to 256
// (one ublk device per universe plus one per configured device), so a node left
// at the default will refuse to add devices long before racer runs out of
// budget, and it will do so at the worst possible moment: the first time a
// volume is staged.
func checkUblksMax() error {
	raw, err := os.ReadFile(ublksMaxPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is missing: the ublk_drv module is not loaded", ublksMaxPath)
		}

		return fmt.Errorf("read %s: %w", ublksMaxPath, err)
	}

	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("parse %s: %q is not a number", ublksMaxPath, strings.TrimSpace(string(raw)))
	}

	if value < racerctrl.MaxExports {
		return fmt.Errorf(
			"%s is %d, below racer's export budget of %d: reload the module with ublks_max=%d "+
				"(modprobe ublk_drv ublks_max=%d, or set it in /etc/modprobe.d)",
			ublksMaxPath, value, racerctrl.MaxExports, racerctrl.MaxExports, racerctrl.MaxExports,
		)
	}

	return nil
}

// checkStore confirms the filesystem the backing store lives on honours both
// O_DIRECT and RWF_DSYNC. racer depends on each: O_DIRECT because it manages
// its own cache and must not have the page cache lie to it about durability,
// and RWF_DSYNC because a page register commit is only meaningful once the
// write has reached stable media. Several filesystems racer might plausibly be
// pointed at (tmpfs, overlayfs, some network filesystems) silently fail one or
// both, and the failure mode without this check is data loss on power cut
// rather than an error.
func checkStore(storePath string) error {
	dir := filepath.Dir(storePath)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create store directory %s: %w", dir, err)
	}

	probe := filepath.Join(dir, ".racer-preflight")

	// Remove a probe left behind by a killed earlier run before creating ours,
	// so a stale file cannot make the check fail forever.
	_ = os.Remove(probe) //nolint:errcheck

	fd, err := unix.Open(probe, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_DIRECT, 0o600)
	if err != nil {
		if errors.Is(err, unix.EINVAL) {
			return fmt.Errorf(
				"filesystem holding %s does not support O_DIRECT: point %s at a real block-backed filesystem "+
					"(tmpfs and overlayfs do not qualify)",
				storePath, EnvStorePath,
			)
		}

		return fmt.Errorf("open %s with O_DIRECT: %w", probe, err)
	}

	defer func() {
		_ = unix.Close(fd)   //nolint:errcheck
		_ = os.Remove(probe) //nolint:errcheck
	}()

	buf := alignedBlock()

	n, err := unix.Pwritev2(fd, [][]byte{buf}, 0, unix.RWF_DSYNC)
	if err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			return fmt.Errorf(
				"filesystem holding %s does not support RWF_DSYNC writes: %w", storePath, err,
			)
		}

		return fmt.Errorf("O_DIRECT RWF_DSYNC write to %s: %w", probe, err)
	}

	if n != len(buf) {
		return fmt.Errorf("short O_DIRECT write to %s: wrote %d of %d bytes", probe, n, len(buf))
	}

	return nil
}

// checkNvmet confirms the kernel NVMe target is available through configfs.
// Publishing a namespace is how a universe's members reach each other's page
// registers, so without it a multi-node universe cannot be assembled.
func checkNvmet(root string) error {
	info, err := os.Stat(filepath.Join(root, "subsystems"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"%s/subsystems is missing: mount configfs and load the target modules "+
					"(modprobe nvmet nvmet_tcp; mount -t configfs none /sys/kernel/config)",
				root,
			)
		}

		return fmt.Errorf("stat %s/subsystems: %w", root, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s/subsystems is not a directory", root)
	}

	return nil
}

// alignedBlock returns a 4 KiB buffer whose backing memory starts on a 4 KiB
// boundary. O_DIRECT requires the buffer address, the file offset and the
// length all be aligned to the device's logical block size; 4 KiB satisfies
// every block size racer will meet.
func alignedBlock() []byte {
	raw := make([]byte, 2*directProbeSize)
	offset := int(uintptr(unsafe.Pointer(&raw[0])) % directProbeSize)

	if offset != 0 {
		offset = directProbeSize - offset
	}

	return raw[offset : offset+directProbeSize]
}
