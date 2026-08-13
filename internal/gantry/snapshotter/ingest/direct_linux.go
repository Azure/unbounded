// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package ingest

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenDirect opens a segment device for direct I/O.
//
// O_DIRECT matters here for a reason that is specific to RACER rather than
// generic write performance: the page cache would otherwise hold a second copy
// of every blob this node ingests, and RACER's own store plus its cooperative
// cache is already the cache. Bypassing it also means a write reaches the
// extent's consensus group when WriteAt returns rather than at some later
// writeback, so the catalog record cannot be published before the data lands.
//
// The caller must use AlignedBuffer and whole page offsets and lengths; the
// kernel rejects anything else on an O_DIRECT descriptor with EINVAL.
func OpenDirect(path string) (Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return nil, fmt.Errorf("ingest: open %s: %w", path, err)
	}

	return f, nil
}
