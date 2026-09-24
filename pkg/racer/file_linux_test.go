// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileSpliceAndProgressFallback(t *testing.T) {
	data := payload(2 << 20)

	o := fileObject(t, data)
	for _, mode := range []string{"native", "socket-unsupported", "socket-progress-unsupported", "file-unsupported", "partial-unsupported", "partial-error", "no-progress", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			f := destinationFile(t)
			_, _ = f.Seek(31, io.SeekStart)

			s, _ := o.ReadRange(t.Context(), 17, int64(len(data))-17)
			defer s.Close()

			var fileCalls int

			injected := false
			splice := func(in int, inOff *int64, out int, outOff *int64, length, flags int) (int64, error) {
				if outOff == nil && mode == "socket-progress-unsupported" && fileCalls > 0 {
					injected = true
					return 0, unix.EXDEV
				}

				if outOff == nil && mode == "socket-unsupported" {
					injected = true
					return 0, unix.ENOSYS
				}

				if outOff != nil {
					fileCalls++

					if mode == "file-unsupported" {
						injected = true
						return 0, unix.EOPNOTSUPP
					}

					if mode == "no-progress" {
						injected = true
						return 0, nil
					}

					if mode == "interrupted" && !injected {
						injected = true
						return 0, unix.EINTR
					}

					if mode == "partial-unsupported" || mode == "partial-error" {
						if fileCalls == 1 {
							return unix.Splice(in, inOff, out, outOff, min(length, 137), flags)
						}

						injected = true

						if mode == "partial-error" {
							return 0, unix.ENOSPC
						}

						return 0, unix.EINVAL
					}
				}

				return unix.Splice(in, inOff, out, outOff, length, flags)
			}
			n, err := s.spliceToFileWith(f, 73, splice)
			stats := s.Stats()

			if mode == "partial-error" || mode == "no-progress" {
				want := error(unix.ENOSPC)
				if mode == "no-progress" {
					want = io.ErrNoProgress
				}

				if !errors.Is(err, want) || n >= int64(len(data))-17 {
					t.Fatal(n, err)
				}
			} else if err != nil || n != int64(len(data))-17 {
				t.Fatal(n, err)
			}

			got := make([]byte, n)
			if _, err := f.ReadAt(got, 73); err != nil || !bytes.Equal(got, data[17:17+n]) {
				t.Fatal("lost or repeated progress", n, err)
			}

			if pos, _ := f.Seek(0, io.SeekCurrent); pos != 31 {
				t.Fatal("cursor moved", pos)
			}

			if mode == "native" && (stats.SpliceBytes < 1<<20 || stats.SpliceCalls == 0) {
				t.Fatal("no real fast path", stats)
			}

			if mode != "native" && !injected {
				t.Fatal("fault not exercised")
			}

			if err == nil && stats.SpliceBytes+stats.BufferedBytes != n {
				t.Fatal("accounting", stats, n)
			}

			if mode == "partial-unsupported" && stats.SpliceBytes != 137 {
				t.Fatal("missing partial splice", stats)
			}

			assertAdmissionFree(t, o.client)

			if err == nil && len(o.client.streamPool.pipes.idle) == 0 {
				t.Fatal("drained pipe not cached")
			}
		})
	}
}
