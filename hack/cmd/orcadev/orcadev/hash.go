// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// hasher returns a fresh streaming SHA-256 hasher; used to compute
// digests as bytes flow through (no buffering of large objects).
func hasher() hash.Hash { return sha256.New() }

// hexSum returns the hex-encoded final digest of h.
func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// teeHashReader wraps r so every byte read flows through h before
// being returned. Read returns r.Read's results unchanged. Closing
// it closes the underlying ReadCloser if it implements io.Closer.
type teeHashReader struct {
	r io.Reader
	h hash.Hash
}

// newTeeHashReader returns a reader that streams r through h. The
// caller is responsible for retaining h to read out the final
// digest after the stream is drained.
func newTeeHashReader(r io.Reader, h hash.Hash) io.Reader {
	return &teeHashReader{r: r, h: h}
}

func (t *teeHashReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_, _ = t.h.Write(p[:n]) //nolint:errcheck // hash.Hash never returns an error from Write
	}

	return n, err
}

// firstDiffOffset returns the index of the first byte at which a and b
// differ. Returns -1 when the two slices are byte-identical for the
// length of the shorter slice; returns min(len(a), len(b)) when the
// shorter slice is a prefix of the longer slice (length difference is
// itself a difference).
func firstDiffOffset(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}

	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}

	if len(a) == len(b) {
		return -1
	}

	return n
}

// hexDiffDump formats a side-by-side hex diff of a vs b starting at
// startOffset, capped at width bytes. Each line shows offset, the
// "source" bytes from a, the "received" bytes from b, and ASCII
// renderings. Used by `roundtrip --dump-diff` to show why a checksum
// mismatch happened without dumping unbounded data.
func hexDiffDump(a, b []byte, startOffset, width int) string {
	if width <= 0 {
		width = 16
	}

	const cols = 16 // bytes per row

	var sb strings.Builder

	fmt.Fprintf(&sb, "offset 0x%x (%d):\n", startOffset, startOffset) //nolint:errcheck // strings.Builder never errors
	sb.WriteString("           SOURCE                                          | RECEIVED\n")

	end := startOffset + width
	for off := startOffset; off < end; off += cols {
		fmt.Fprintf(&sb, "  %08x  %s | %s\n", //nolint:errcheck // strings.Builder never errors
			off,
			renderRow(a, off, cols),
			renderRow(b, off, cols),
		)
	}

	return sb.String()
}

// renderRow formats a single hex-dump row: "<hex-bytes>  <ascii>".
// Out-of-range bytes are rendered as "  " (hex) / " " (ascii) so the
// two columns of hexDiffDump align even when the slices have
// different lengths.
func renderRow(b []byte, off, cols int) string {
	var hexPart, asciiPart strings.Builder

	for i := 0; i < cols; i++ {
		idx := off + i
		if idx < len(b) {
			fmt.Fprintf(&hexPart, "%02x ", b[idx]) //nolint:errcheck // strings.Builder never errors

			c := b[idx]
			if c >= 0x20 && c < 0x7f {
				asciiPart.WriteByte(c)
			} else {
				asciiPart.WriteByte('.')
			}
		} else {
			hexPart.WriteString("   ")
			asciiPart.WriteByte(' ')
		}
	}

	return hexPart.String() + " " + asciiPart.String()
}
