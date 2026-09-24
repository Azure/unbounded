// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"io"
	"testing"
	"time"
)

func TestStreamPrefetchSpliceAcrossPages(t *testing.T) {
	c := newTestClient(t, prefetchFixture(t, PageSize+(1<<20), nil), ClientOptions{StreamPrefetch: true, MaxActiveRequests: 2})
	s := openPrefetchRange(t, c, t.Context(), PageSize-(1<<20), 2<<20)

	dst, reader := downstreamPair(t, "unix")
	if err := dst.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := reader.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)

	go func() {
		v := &prefetchVerifier{offset: PageSize - (1 << 20)}

		n, err := io.Copy(v, io.LimitReader(reader, 2<<20))
		if err == nil && n != 2<<20 {
			err = io.ErrUnexpectedEOF
		}

		result <- err
	}()

	if n, err := s.WriteTo(dst); n != 2<<20 || err != nil {
		t.Fatal(n, err)
	}

	if err := <-result; err != nil {
		t.Fatal(err)
	}

	if s.Stats().SpliceBytes == 0 {
		t.Fatal("prefetch bypassed splice")
	}

	if len(c.admission.slots) != 0 {
		t.Fatal("permits leaked")
	}
}
