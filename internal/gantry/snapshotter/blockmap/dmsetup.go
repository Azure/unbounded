// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package blockmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DevmapperPath is where device mapper devices appear.
const DevmapperPath = "/dev/mapper"

// dmsetupTimeout bounds a single dmsetup invocation. Device mapper operations
// are ioctls that either complete in microseconds or are stuck behind a device
// that is not answering, and blocking the container start path forever on the
// second case is worse than failing.
const dmsetupTimeout = 30 * time.Second

// Dmsetup drives device mapper through the dmsetup binary.
//
// Shelling out costs a fork per mapping, which is on the first-container path
// for a layer but not on any subsequent one. The ioctl interface would avoid
// it; that is a worthwhile change once the rest of the path is measured, and
// the Devmapper interface exists so it can be made without touching callers.
type Dmsetup struct {
	// Binary is the dmsetup executable. Empty means look it up on PATH.
	Binary string

	// Dir is where device nodes appear.
	Dir string
}

// NewDmsetup returns a Dmsetup with the usual paths.
func NewDmsetup() *Dmsetup { return &Dmsetup{Binary: "dmsetup", Dir: DevmapperPath} }

// Path implements Devmapper.
func (d *Dmsetup) Path(name string) string {
	return filepath.Join(orDefault(d.Dir, DevmapperPath), name)
}

// Create implements Devmapper.
//
// The device is created read-only. The blob behind it is in an immutable
// extent, so a write would fail at the bottom of the stack anyway; failing at
// the top instead means a bug shows up as EROFS on the mount rather than as an
// I/O error under a filesystem.
//
// udev synchronization is disabled. Waiting for udev adds tens of milliseconds
// per layer to the first container that uses an image, and nothing here needs
// the symlinks udev would create: the device node dmsetup makes itself is what
// gets mounted.
func (d *Dmsetup) Create(ctx context.Context, name string, table Table) error {
	if err := table.Validate(); err != nil {
		return err
	}

	if err := validName(name); err != nil {
		return err
	}

	if _, err := d.run(ctx, "create", name, "--readonly", "--noudevsync", "--table", table.String()); err != nil {
		return err
	}

	// dmsetup returns before the node is guaranteed to be visible when udev
	// synchronization is off. Poll rather than sleep a fixed interval: this
	// is normally satisfied on the first check.
	return waitFor(ctx, d.Path(name))
}

// Remove implements Devmapper.
func (d *Dmsetup) Remove(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}

	exists, err := d.Exists(ctx, name)
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	// --retry handles the window where a mount has just gone away but the
	// last opener has not been released yet.
	_, err = d.run(ctx, "remove", "--retry", "--noudevsync", name)

	return err
}

// Exists implements Devmapper.
func (d *Dmsetup) Exists(ctx context.Context, name string) (bool, error) {
	if err := validName(name); err != nil {
		return false, err
	}

	_, err := d.run(ctx, "info", "--noheadings", "-c", "-o", "name", name)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// Names implements Devmapper.
func (d *Dmsetup) Names(ctx context.Context, prefix string) ([]string, error) {
	out, err := d.run(ctx, "ls", "--target", "linear")
	if err != nil {
		return nil, err
	}

	var names []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		// Each line is "<name>\t(<major>:<minor>)". An empty table prints
		// "No devices found".
		name, _, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}

		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}

	return names, nil
}

func (d *Dmsetup) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dmsetupTimeout)
	defer cancel()

	binary := orDefault(d.Binary, "dmsetup")

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("dmsetup %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// validName rejects anything that is not a plain device mapper name. Names are
// built from digests, so anything else means a caller passed through data it
// should not have, and it would end up in a command line.
func validName(name string) error {
	if name == "" || len(name) > 127 {
		return fmt.Errorf("blockmap: device name %q is not between 1 and 127 characters", name)
	}

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("blockmap: device name %q contains %q", name, r)
		}
	}

	return nil
}

// waitFor polls for a path to appear.
func waitFor(ctx context.Context, path string) error {
	const (
		interval = 200 * time.Microsecond
		limit    = 5 * time.Second
	)

	deadline := time.Now().Add(limit)

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("blockmap: %s did not appear within %s", path, limit)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
