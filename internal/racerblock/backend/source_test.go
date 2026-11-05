// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

// versionedOrigin serves byte-pattern versions keyed by ETag. An empty request
// ETag selects current.
type versionedOrigin struct {
	mu       sync.Mutex
	current  string
	versions map[string][]byte
}

func (o *versionedOrigin) setCurrent(etag string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.current = etag
}

func (o *versionedOrigin) serve(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	etag := r.ETag
	if etag == "" {
		etag = o.current
	}

	data, ok := o.versions[etag]
	if !ok {
		return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
	}

	md := racersdk.Metadata{Size: int64(len(data)), ETag: etag, ExpiresAt: time.Now().Add(time.Hour)}
	if r.Head {
		return md, nil, nil
	}

	start := min(r.Offset, md.Size)
	end := min(start+r.Length, md.Size)

	return md, io.NopCloser(bytes.NewReader(data[start:end])), nil
}

func filled(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func readAt(t *testing.T, src Source, off, n int64, etag string) ([]byte, error) {
	t.Helper()

	r, err := src.Open(context.Background(), off, n, etag)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return io.ReadAll(r)
}

// A pinned older version larger than one page must not inherit the small-object
// hint from a smaller current version.
func TestFromClientPinnedLargeVersionAfterSmallStat(t *testing.T) {
	o := &versionedOrigin{current: `"new"`, versions: map[string][]byte{
		`"old"`: filled(int(racersdk.PageSize)+4096, 'o'),
		`"new"`: filled(100, 'n'),
	}}
	src := FromClient(racersdktest.NewClient(t, o.serve), racersdk.Request{})

	md, err := src.Stat(context.Background())
	if err != nil || md.ETag != `"new"` || md.Size != 100 {
		t.Fatal("stat", md, err)
	}

	off := int64(racersdk.PageSize) + 10

	got, err := readAt(t, src, off, 8, `"old"`)
	if err != nil || !bytes.Equal(got, filled(8, 'o')) {
		t.Fatalf("pinned read of older large version: %q %v", got, err)
	}

	// The small current version still reads correctly with its own hint.
	got, err = readAt(t, src, 0, 100, `"new"`)
	if err != nil || !bytes.Equal(got, filled(100, 'n')) {
		t.Fatalf("small version read: %q %v", got, err)
	}

	// A missing version still reports a mismatch.
	if _, err := readAt(t, src, 0, 1, `"gone"`); err == nil {
		t.Fatal("expected error for missing version")
	}
}

// Warm refreshing to a small version must not affect reads pinned elsewhere,
// and unpinned reads never claim SmallObject.
func TestFromClientWarmHintBoundToETag(t *testing.T) {
	o := &versionedOrigin{current: `"big"`, versions: map[string][]byte{
		`"big"`:   filled(int(racersdk.PageSize)*2, 'b'),
		`"small"`: filled(10, 's'),
	}}
	src := FromClient(racersdktest.NewClient(t, o.serve), racersdk.Request{})

	o.setCurrent(`"small"`)

	if _, err := src.Warm(context.Background(), time.Minute, ""); err != nil {
		t.Fatal(err)
	}

	got, err := readAt(t, src, int64(racersdk.PageSize), 4, `"big"`)
	if err != nil || !bytes.Equal(got, filled(4, 'b')) {
		t.Fatalf("pinned big read after small warm: %q %v", got, err)
	}

	o.setCurrent(`"big"`)

	got, err = readAt(t, src, int64(racersdk.PageSize), 4, "")
	if err != nil || !bytes.Equal(got, filled(4, 'b')) {
		t.Fatalf("unpinned read: %q %v", got, err)
	}
}

func TestClientSourceSmallObject(t *testing.T) {
	s := &clientSource{}
	if s.smallObject(`"a"`) {
		t.Fatal("unknown size must not be small")
	}

	s.remember(racersdk.Metadata{ETag: `"a"`, Size: 10})

	for _, tc := range []struct {
		etag string
		want bool
	}{{`"a"`, true}, {`"b"`, false}, {"", false}} {
		if got := s.smallObject(tc.etag); got != tc.want {
			t.Errorf("smallObject(%q) = %v", tc.etag, got)
		}
	}

	s.remember(racersdk.Metadata{ETag: `"a"`, Size: int64(racersdk.PageSize) + 1})

	if s.smallObject(`"a"`) {
		t.Fatal("large version must not be small")
	}

	s.remember(racersdk.Metadata{ETag: `"a"`, Size: int64(racersdk.PageSize)})

	if !s.smallObject(`"a"`) {
		t.Fatal("exactly one page must be small")
	}
}
