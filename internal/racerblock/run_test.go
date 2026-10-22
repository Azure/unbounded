// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerblock

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racerblock/backend"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

var testKey = racersdk.Key{0xab, 0xcd}

// patternPeriod is prime, so the pattern never lines up with sectors,
// chunks, or pages and a read from the wrong offset shows up as a mismatch.
const patternPeriod = 4093

// patternTable holds one period of the object content.
var patternTable = func() []byte {
	t := make([]byte, 2*patternPeriod)
	for i := range t {
		t[i] = byte(i%patternPeriod*7 + i%patternPeriod>>5)
	}

	return t
}()

// fillPattern writes the object content for [off, off+len(p)) into p.
func fillPattern(p []byte, off int64) {
	for len(p) > 0 {
		start := off % patternPeriod
		n := copy(p, patternTable[start:start+patternPeriod])
		p = p[n:]
		off += int64(n)
	}
}

type patternReader struct {
	off, end int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), r.end-r.off))
	fillPattern(p[:n], r.off)

	r.off += int64(n)

	return n, nil
}

// patternOrigin serves pattern data of the given size as version "v1".
func patternOrigin(size int64) racersdk.Origin {
	return func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		md := racersdk.Metadata{Size: size, ETag: `"v1"`, ExpiresAt: time.Now().Add(time.Hour)}

		if r.Key != testKey {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}

		if r.ETag != "" && r.ETag != md.ETag {
			return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
		}

		if r.Head {
			return md, nil, nil
		}

		start := min(r.Offset, size)
		end := min(start+r.Length, size)

		return md, io.NopCloser(&patternReader{off: start, end: end}), nil
	}
}

func TestRunRequiresSource(t *testing.T) {
	t.Parallel()

	if err := Run(t.Context(), Config{}); err == nil {
		t.Fatal("Run without Source succeeded")
	}
}

// Open failures happen before any ublk call, so these run without root.
func TestRunOpenErrors(t *testing.T) {
	t.Parallel()

	client := racersdktest.NewClient(t, patternOrigin(1<<20))

	tests := []struct {
		name string
		key  racersdk.Key
		etag string
		want error
	}{
		{name: "missing object", key: racersdk.Key{1}, want: racersdk.ErrNotFound},
		{name: "pinned version gone", key: testKey, etag: `"v0"`, want: racersdk.ErrVersionMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ready := false

			err := Run(t.Context(), Config{
				Source:  backend.FromClient(client, racersdk.Request{Key: tt.key}),
				Backend: backend.Config{ETag: tt.etag},
				Ready:   func(string) { ready = true },
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run error = %v, want %v", err, tt.want)
			}

			if ready {
				t.Fatal("Ready called after a failed open")
			}
		})
	}
}
