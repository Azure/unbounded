// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package catalog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DefaultConflictErrnos are the errnos treated as an OCC compare-and-swap
// failure on the catalog device.
//
// EAGAIN is the one RACER actually reports. A conflicting write fails with
// Status::Conflict, which Status::errno maps to EAGAIN before the ublk server
// answers the I/O; the block layer round-trips that through BLK_STS_AGAIN back
// to EAGAIN, and a write to a non-pollable file descriptor surfaces it
// unchanged. A conflict detected by a peer across the fabric travels as
// EREMOTEIO, but Status::from_wire converts it back to Status::Conflict before
// the consumer side ever sees it, so EREMOTEIO does not reach us.
//
// EBUSY is here as a hedge against a future device reporting the same
// condition differently, and the set is configurable for the same reason.
// Getting it wrong is not a correctness problem in the dangerous direction: an
// unrecognised conflict surfaces as a plain I/O error and the ingest is
// retried later, whereas a recognised one is retried immediately against a
// re-read block. What must never happen is treating a successful write as a
// conflict, and no kernel reports success as an errno.
var DefaultConflictErrnos = []unix.Errno{unix.EAGAIN, unix.EBUSY}

// ParseConflictErrnos turns a comma-separated list of errno names or numbers
// into a set usable by OpenDevice. An empty string yields the defaults.
func ParseConflictErrnos(s string) ([]unix.Errno, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultConflictErrnos, nil
	}

	names := map[string]unix.Errno{
		"EAGAIN":      unix.EAGAIN,
		"EWOULDBLOCK": unix.EWOULDBLOCK,
		"EBUSY":       unix.EBUSY,
		"EIO":         unix.EIO,
		"ESTALE":      unix.ESTALE,
		"EEXIST":      unix.EEXIST,
		"ECANCELED":   unix.ECANCELED,
		"EREMOTEIO":   unix.EREMOTEIO,
	}

	var out []unix.Errno

	for _, field := range strings.Split(s, ",") {
		field = strings.ToUpper(strings.TrimSpace(field))
		if field == "" {
			continue
		}

		if e, ok := names[field]; ok {
			out = append(out, e)
			continue
		}

		n, err := strconv.Atoi(field)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("catalog: unknown errno %q", field)
		}

		out = append(out, unix.Errno(n))
	}

	if len(out) == 0 {
		return DefaultConflictErrnos, nil
	}

	return out, nil
}

// Device is a Volume backed by a block device opened with O_DIRECT.
//
// O_DIRECT is not an optimisation here, it is the point. The page cache would
// happily serve a stale copy of a block another node has since rewritten,
// which for an optimistically concurrent index means reading a version that
// was never current and then writing on top of it. Bypassing the cache means
// every read reaches RACER and so re-arms the OCC guard the following write is
// checked against.
//
// O_DIRECT in turn demands that the buffer, the offset and the length all be
// aligned. Every Store access is exactly one block at a block-aligned offset,
// so the only thing left to arrange is the buffer, which is why reads and
// writes bounce through an aligned scratch block rather than using the
// caller's slice directly.
type Device struct {
	f         *os.File
	conflicts []unix.Errno

	// mu guards scratch. Store already serializes device access for OCC
	// reasons, but a Device is an ordinary io type and must not corrupt
	// itself if something else uses it concurrently.
	mu      sync.Mutex
	scratch []byte
}

// DeviceOptions configures OpenDevice.
type DeviceOptions struct {
	// ConflictErrnos overrides DefaultConflictErrnos.
	ConflictErrnos []unix.Errno

	// Direct opens the device with O_DIRECT. It defaults to true and only
	// exists so tests can point a Device at an ordinary file on a
	// filesystem that refuses O_DIRECT.
	Direct *bool
}

// OpenDevice opens the catalog device at path.
func OpenDevice(path string, opts DeviceOptions) (*Device, error) {
	if path == "" {
		return nil, errors.New("catalog: device path required")
	}

	flags := os.O_RDWR
	if opts.Direct == nil || *opts.Direct {
		flags |= unix.O_DIRECT
	}

	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("catalog: open %s: %w", path, err)
	}

	conflicts := opts.ConflictErrnos
	if len(conflicts) == 0 {
		conflicts = DefaultConflictErrnos
	}

	return &Device{
		f:         f,
		conflicts: conflicts,
		scratch:   alignedBlock(),
	}, nil
}

// Close releases the device.
func (d *Device) Close() error {
	if d == nil || d.f == nil {
		return nil
	}

	return d.f.Close()
}

// Name returns the device path, for logs.
func (d *Device) Name() string {
	if d == nil || d.f == nil {
		return ""
	}

	return d.f.Name()
}

// ReadAt implements Volume.
func (d *Device) ReadAt(p []byte, off int64) (int, error) {
	if err := checkAccess(p, off); err != nil {
		return 0, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	n, err := d.f.ReadAt(d.scratch, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, d.translate(err)
	}

	if n < BlockBytes {
		return 0, fmt.Errorf("catalog: short read of %d bytes at %d: %w", n, off, io.ErrUnexpectedEOF)
	}

	copy(p, d.scratch)

	return BlockBytes, nil
}

// WriteAt implements Volume. A conflicting write is reported as ErrConflict so
// the Store retries it against a freshly read block.
func (d *Device) WriteAt(p []byte, off int64) (int, error) {
	if err := checkAccess(p, off); err != nil {
		return 0, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	copy(d.scratch, p)

	n, err := d.f.WriteAt(d.scratch, off)
	if err != nil {
		return 0, d.translate(err)
	}

	if n < BlockBytes {
		return 0, fmt.Errorf("catalog: short write of %d bytes at %d", n, off)
	}

	return BlockBytes, nil
}

// translate maps a device error onto ErrConflict when it is one of the errnos
// RACER uses to report a failed compare-and-swap.
func (d *Device) translate(err error) error {
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return err
	}

	for _, c := range d.conflicts {
		if errno == c {
			return fmt.Errorf("%w: %v", ErrConflict, errno)
		}
	}

	return err
}

// checkAccess enforces the contract O_DIRECT and OCC share: whole blocks at
// block-aligned offsets, nothing else.
func checkAccess(p []byte, off int64) error {
	if len(p) != BlockBytes {
		return fmt.Errorf("catalog: access of %d bytes, want %d", len(p), BlockBytes)
	}

	if off < 0 || off%BlockBytes != 0 {
		return fmt.Errorf("catalog: unaligned offset %d", off)
	}

	return nil
}

// alignedBlock returns a block-sized buffer starting on a block boundary,
// which is what O_DIRECT requires of the memory it reads from and writes into.
func alignedBlock() []byte {
	buf := make([]byte, 2*BlockBytes)

	pad := int(uintptr(unsafe.Pointer(&buf[0])) % BlockBytes)
	if pad != 0 {
		pad = BlockBytes - pad
	}

	return buf[pad : pad+BlockBytes : pad+BlockBytes]
}
