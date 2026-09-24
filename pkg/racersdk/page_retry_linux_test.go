// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPageRetryConcurrentCloseReleasesResources(t *testing.T) {
	var gets atomic.Int32

	failed := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprint(PageSize+1))
			return
		}

		gets.Add(1)

		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", PageSize, PageSize) {
			w.Header().Set("Content-Length", "0")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			w.(http.Flusher).Flush()
			// The retry must close this rejected connection before waiting. Only
			// then tell the test to cancel concurrently during the backoff.
			<-r.Context().Done()
			close(failed)

			return
		}

		w.Header().Set("Content-Length", "131072")
		w.Header().Set("Content-Range", contentRange(PageSize-131072, PageSize-1, PageSize+1))
		w.WriteHeader(206)
		_, _ = w.Write(make([]byte, 131072))
	}), ClientOptions{})

	o, err := c.Open(t.Context(), "/object")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(t.Context(), PageSize-131072, 131073)
	if err != nil {
		t.Fatal(err)
	}

	dst, receiver := downstreamPair(t, "tcp")
	readDone := make(chan struct{})

	go func() { _, _ = io.Copy(io.Discard, receiver); close(readDone) }()

	done := make(chan error, 1)

	go func() { _, err := s.WriteTo(dst); done <- err }()

	select {
	case <-failed:
	case <-time.After(3 * time.Second):
		t.Fatal("no second page")
	}

	closed := make(chan struct{}, 4)

	for range 4 {
		go func() { _ = s.Close(); closed <- struct{}{} }()
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry wait ignored cancellation")
	}

	_ = dst.Close()

	<-readDone

	for range 4 {
		<-closed
	}

	_ = s.Close()
	if s.conn != nil || len(c.streamPool.idle) != 0 || len(c.streamPool.pipes.idle) != 0 || gets.Load() != 2 || s.Stats().SpliceBytes == 0 {
		t.Fatal("retained failed connection/pipe or replayed page", gets.Load(), s.Stats())
	}
}
