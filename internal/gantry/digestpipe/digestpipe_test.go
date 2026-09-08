// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package digestpipe_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/digestpipe"
)

func TestWriter_PassThroughAndDigest(t *testing.T) {
	payload := []byte("hello, gantry digestpipe!")
	want := sha256.Sum256(payload)
	wantDigest := digest.MustParse("sha256:" + hex.EncodeToString(want[:]))

	var sink bytes.Buffer

	w := digestpipe.New(&sink)

	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != len(payload) {
		t.Fatalf("short write: got %d want %d", n, len(payload))
	}

	if !bytes.Equal(sink.Bytes(), payload) {
		t.Fatalf("destination got %q; want %q", sink.String(), payload)
	}

	if got, want := w.Written(), int64(len(payload)); got != want {
		t.Fatalf("Written: got %d want %d", got, want)
	}

	if err := w.Verify(wantDigest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestWriter_VerifyMismatch(t *testing.T) {
	w := digestpipe.New(io.Discard)
	if _, err := w.Write([]byte("not the right bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Some valid-shaped but unrelated digest.
	bogus := digest.MustParse("sha256:" + strings.Repeat("ab", 32))

	err := w.Verify(bogus)
	if err == nil {
		t.Fatalf("Verify: expected mismatch error, got nil")
	}

	if !errors.Is(err, digestpipe.ErrDigestMismatch) {
		t.Fatalf("Verify err is not ErrDigestMismatch: %v", err)
	}

	if !strings.Contains(err.Error(), bogus.String()) {
		t.Fatalf("error %q does not mention expected digest %q", err.Error(), bogus.String())
	}
}

func TestWriter_MultipleWrites(t *testing.T) {
	chunks := [][]byte{
		[]byte("first chunk; "),
		[]byte("second chunk; "),
		[]byte("third"),
	}
	full := bytes.Join(chunks, nil)
	wantSum := sha256.Sum256(full)
	wantDigest := digest.MustParse("sha256:" + hex.EncodeToString(wantSum[:]))

	var sink bytes.Buffer

	w := digestpipe.New(&sink)
	for _, c := range chunks {
		n, err := w.Write(c)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		if n != len(c) {
			t.Fatalf("short write: got %d want %d", n, len(c))
		}
	}

	if !bytes.Equal(sink.Bytes(), full) {
		t.Fatalf("destination got %q; want %q", sink.String(), full)
	}

	if err := w.Verify(wantDigest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestWriter_NilDestination(t *testing.T) {
	// A nil dst must not panic; Writer should fall back to io.Discard.
	w := digestpipe.New(nil)
	if _, err := w.Write([]byte("test")); err != nil {
		t.Fatalf("Write to nil-dst pipe: %v", err)
	}

	wantSum := sha256.Sum256([]byte("test"))

	wantDigest := digest.MustParse("sha256:" + hex.EncodeToString(wantSum[:]))
	if err := w.Verify(wantDigest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// errWriter is an io.Writer that fails after writing limit bytes.
type errWriter struct {
	buf   bytes.Buffer
	limit int
}

func (e *errWriter) Write(p []byte) (int, error) {
	rem := e.limit - e.buf.Len()
	if rem <= 0 {
		return 0, io.ErrShortWrite
	}

	if len(p) <= rem {
		return e.buf.Write(p)
	}

	n, _ := e.buf.Write(p[:rem])

	return n, io.ErrShortWrite
}

func TestWriter_ShortWritePropagated(t *testing.T) {
	dst := &errWriter{limit: 3}
	w := digestpipe.New(dst)

	n, err := w.Write([]byte("hello"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write err: got %v want %v", err, io.ErrShortWrite)
	}

	if n != 3 {
		t.Fatalf("Write n: got %d want 3", n)
	}
	// Hash must cover only the accepted prefix ("hel").
	wantSum := sha256.Sum256([]byte("hel"))

	wantDigest := digest.MustParse("sha256:" + hex.EncodeToString(wantSum[:]))
	if err := w.Verify(wantDigest); err != nil {
		t.Fatalf("Verify of accepted prefix: %v", err)
	}
}
