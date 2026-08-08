// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package streamcopy

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type trackingReader struct {
	reader  *bytes.Reader
	reads   int
	largest int
}

type countingReaderAt struct {
	reads int
	size  int64
}

func (r *countingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.reads++

	remaining := r.size - offset
	if remaining <= 0 {
		return 0, io.EOF
	}

	read := min(int64(len(p)), remaining)
	for index := range p[:read] {
		p[index] = 'x'
	}

	if read < int64(len(p)) {
		return int(read), io.EOF
	}

	return int(read), nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (r *trackingReader) Read(p []byte) (int, error) {
	r.reads++
	if len(p) > r.largest {
		r.largest = len(p)
	}

	return r.reader.Read(p)
}

func TestCopyNUsesLargeBoundedChunks(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 2*BufferSize+17)
	source := &trackingReader{reader: bytes.NewReader(body)}

	var destination bytes.Buffer

	written, err := CopyN(&destination, source, int64(len(body)))
	if err != nil {
		t.Fatalf("CopyN: %v", err)
	}

	if written != int64(len(body)) || !bytes.Equal(destination.Bytes(), body) {
		t.Fatalf("copied %d bytes, want %d", written, len(body))
	}

	if source.reads != 3 || source.largest != BufferSize {
		t.Fatalf("reads = %d, largest = %d; want 3 reads of at most %d", source.reads, source.largest, BufferSize)
	}
}

func TestCopyNPreservesShortSource(t *testing.T) {
	var destination bytes.Buffer

	written, err := CopyN(&destination, bytes.NewReader([]byte("short")), 10)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("CopyN error = %v, want ErrUnexpectedEOF", err)
	}

	if written != 5 || destination.String() != "short" {
		t.Fatalf("CopyN = %d bytes %q", written, destination.String())
	}
}

func TestCopyNRejectsNegativeSize(t *testing.T) {
	if _, err := CopyN(io.Discard, bytes.NewReader(nil), -1); err == nil {
		t.Fatal("CopyN accepted a negative size")
	}
}

func TestCopyNReducesSectionReaderCalls(t *testing.T) {
	const size = 32 * BufferSize

	optimizedSource := &countingReaderAt{size: size}

	written, err := CopyN(discardWriter{}, io.NewSectionReader(optimizedSource, 0, size), size)
	if err != nil || written != size {
		t.Fatalf("optimized CopyN = %d, %v", written, err)
	}

	defaultSource := &countingReaderAt{size: size}

	written, err = io.Copy(discardWriter{}, io.NewSectionReader(defaultSource, 0, size))
	if err != nil || written != size {
		t.Fatalf("default io.Copy = %d, %v", written, err)
	}

	if optimizedSource.reads != 32 || defaultSource.reads != 1024 {
		t.Fatalf("ReaderAt calls: optimized=%d default=%d, want 32 and 1024", optimizedSource.reads, defaultSource.reads)
	}
}
