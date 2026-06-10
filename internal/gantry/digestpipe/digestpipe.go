// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package digestpipe is the digest-verifying stream tee.
//
// A Writer wraps an underlying io.Writer (typically a *os.File or an
// io.MultiWriter that fans out to disk + a peer response) and computes
// a sha256 digest incrementally as bytes flow through. After the stream
// ends the caller invokes Verify(d) to check that the computed digest
// matches an expected value.
//
// The original the design doc design called for a "digest-verifying stream tee
// (containerd ↔ peer ↔ cache)". This package is the primitive on which
// containerdstore.Writer's digest verification is built; it can also
// be composed directly by callers that already own the destination
// io.Writer (peer transfer, HTTP response). The tee itself is just
// bytes-through; Verify is the authoritative go/no-go.
package digestpipe

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

// Writer is an io.Writer that forwards bytes to a destination while
// accumulating a sha256 digest. Use one Writer per stream; not safe
// for concurrent Write/Verify.
type Writer struct {
	dst     io.Writer
	h       hash.Hash
	written int64
}

// New returns a Writer that forwards bytes to dst while accumulating
// a sha256 digest. dst must be non-nil.
func New(dst io.Writer) *Writer {
	if dst == nil {
		// io.Discard keeps Write infallible if a caller wires nil
		// by mistake. Verify will still operate on zero bytes which
		// will never match a real content digest.
		dst = io.Discard
	}

	return &Writer{dst: dst, h: sha256.New()}
}

// Write forwards p to the underlying destination, then folds the
// successfully-written prefix p[:n] into the running hash. Bytes that
// the destination rejected (n < len(p)) are not folded in, so callers
// that retry must restart from a fresh Writer.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		_, _ = w.h.Write(p[:n]) //nolint:errcheck // best-effort write
		w.written += int64(n)
	}

	return n, err
}

// Written returns the number of bytes successfully forwarded so far.
func (w *Writer) Written() int64 { return w.written }

// Sum returns the lowercase hex sha256 of bytes seen so far. Calling
// Sum does not advance or reset internal state and is safe to invoke
// repeatedly.
func (w *Writer) Sum() string {
	return hex.EncodeToString(w.h.Sum(nil))
}

// ErrDigestMismatch is returned by Verify when the computed digest
// does not match the expected one. Callers can errors.Is against
// this sentinel for retry/fail-class logic.
var ErrDigestMismatch = errors.New("digestpipe: digest mismatch")

// Verify returns nil when the computed sha256 matches want. On
// mismatch it returns an error that wraps ErrDigestMismatch and
// includes both the expected and observed digests in the message.
func (w *Writer) Verify(want digest.Digest) error {
	got := w.Sum()
	if got != want.Hex() {
		return fmt.Errorf("%w: want %s, got sha256:%s",
			ErrDigestMismatch, want.String(), got)
	}

	return nil
}
