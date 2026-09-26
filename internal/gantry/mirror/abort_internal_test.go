// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

type abortRecordingStore struct {
	*fakes.Cache
	writer *abortRecordingWriter
}

func (s *abortRecordingStore) Writer(ctx context.Context, d digest.Digest) (ifaces.ContentWriter, error) {
	w, err := s.Cache.Writer(ctx, d)
	if err != nil {
		return nil, err
	}

	s.writer = &abortRecordingWriter{ContentWriter: w}

	return s.writer, nil
}

type abortRecordingWriter struct {
	ifaces.ContentWriter
	ctx      context.Context
	err      error
	deadline time.Time
	aborted  bool
}

func (w *abortRecordingWriter) Abort(ctx context.Context) error {
	w.ctx = ctx
	w.err = ctx.Err()
	w.deadline, _ = ctx.Deadline()

	if w.err != nil {
		return w.err
	}

	w.aborted = true

	return w.ContentWriter.Abort(ctx)
}

type cancelingPullSource struct {
	ifaces.OriginPuller
	cancel context.CancelFunc
}

func (s cancelingPullSource) Pull(ctx context.Context, _ ifaces.OriginRef) (io.ReadCloser, int64, error) {
	return &cancelingPullReader{ctx: ctx, cancel: s.cancel}, 1024, nil
}

func (s cancelingPullSource) FetchFromPeer(ctx context.Context, _ string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	r, size, err := s.Pull(ctx, ref)

	return r, size, "application/octet-stream", err
}

type cancelingPullReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	read   bool
}

func (r *cancelingPullReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true

		return copy(p, "partial"), nil
	}

	r.cancel()

	return 0, r.ctx.Err()
}

func (*cancelingPullReader) Close() error { return nil }

func TestCanceledPullAbortsWithIndependentBoundedContext(t *testing.T) {
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			store := &abortRecordingStore{Cache: fakes.NewCache()}
			puller := cancelingPullSource{cancel: cancel}
			s := &Server{store: store, origin: puller, peer: puller}
			d := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			response := httptest.NewRecorder()
			started := time.Now()

			if source == "origin" {
				s.serveFromOrigin(ctx, response, d, ifaces.KindBlob, "registry.example.com", "test", logger)
			} else {
				request := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

				result := s.fetchOneProvider(ctx, response, request, d, ifaces.KindBlob, "registry.example.com", "test", ifaces.Provider{Addr: "peer:5001"}, time.Minute, nil, logger)
				if result.outcome != peerFetchOutcomeStall {
					t.Fatalf("outcome = %v; want stall", result.outcome)
				}
			}

			if ctx.Err() != context.Canceled {
				t.Fatal("pull context was not canceled")
			}

			w := store.writer
			if w == nil || w.ctx == nil {
				t.Fatal("cleanup did not call Abort")
			}

			if w.err != nil || !w.aborted {
				t.Fatalf("cleanup failed: %v", w.err)
			}

			if !w.deadline.After(started) || w.deadline.After(time.Now().Add(10*time.Second)) {
				t.Fatalf("cleanup deadline = %v; want independent 10-second budget", w.deadline)
			}

			if w.ctx.Err() != context.Canceled {
				t.Fatal("cleanup context was not canceled after Abort")
			}
		})
	}
}
