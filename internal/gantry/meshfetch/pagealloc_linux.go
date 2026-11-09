// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package meshfetch

import "golang.org/x/sys/unix"

// allocPage maps n bytes of anonymous memory. It falls back to the Go heap
// if mmap fails.
func allocPage(n int) []byte {
	b, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return make([]byte, n)
	}

	return b
}

// freePage unmaps a buffer from allocPage. The caller guarantees that
// nothing references it: page buffers are freed only when their reference
// count drops to zero. Heap fallback buffers are left to the collector.
func freePage(b []byte) {
	if err := unix.Munmap(b); err != nil {
		return
	}
}
