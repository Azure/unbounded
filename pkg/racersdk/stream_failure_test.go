// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStreamFailureLaterPagePreservesStatusAndOffset(t *testing.T) {
	var requests atomic.Int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprint(PageSize+8))
			return
		}

		requests.Add(1)

		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", PageSize-8, PageSize-1) {
			w.Header().Set("Content-Length", "8")
			w.Header().Set("Content-Range", contentRange(PageSize-8, PageSize-1, PageSize+8))
			w.WriteHeader(206)
			_, _ = io.WriteString(w, "12345678")

			return
		}

		w.Header().Set("Content-Length", "0")
		w.WriteHeader(503)
	}), ClientOptions{})

	o, err := c.Open(t.Context(), "/private?token=secret")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(t.Context(), PageSize-8, 16)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Prepare(); err != nil || s.Failure() != nil {
		t.Fatal(err, s.Failure())
	}

	n, err := s.WriteTo(io.Discard)

	var status *HTTPError
	if n != 8 || !errors.As(err, &status) || status.StatusCode != 503 || requests.Load() != 2+pageRetries {
		t.Fatal(n, err, requests.Load())
	}

	_ = s.Close()

	f := s.Failure()
	if f == nil || f.PageOffset != PageSize || f.Offset != PageSize || f.Operation != "page_validate" || f.StatusCode != 503 || !errors.As(f.Err, &status) {
		t.Fatalf("lost first failure: %+v", f)
	}

	f.Offset = 0
	if s.Failure().Offset != PageSize {
		t.Fatal("caller changed retained failure")
	}
}

func TestStreamFailurePrepareAndTruncation(t *testing.T) {
	for _, mode := range []string{"status", "protocol", "truncated", "success"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "8")

				if r.Method == "HEAD" {
					return
				}

				if mode == "status" {
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(504)

					return
				}

				w.Header().Set("Content-Range", "bytes 0-7/8")

				if mode == "protocol" {
					w.Header().Set("Content-Range", "bytes 1-8/9")
				}

				w.WriteHeader(206)

				if mode == "truncated" {
					_, _ = io.WriteString(w, "short")
				} else {
					_, _ = io.WriteString(w, "12345678")
				}
			}), ClientOptions{})

			o, err := c.Open(t.Context(), "/object")
			if err != nil {
				t.Fatal(err)
			}

			s, err := o.Stream(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			err = s.Prepare()
			if err == nil {
				_, err = s.WriteTo(io.Discard)
			}

			f := s.Failure()
			if mode == "success" {
				if err != nil || f != nil {
					t.Fatal(err, f)
				}

				return
			}

			if err == nil || f == nil || f.PageOffset != 0 {
				t.Fatal(err, f)
			}

			switch mode {
			case "status":
				if f.StatusCode != 504 || f.Operation != "page_validate" {
					t.Fatal(f)
				}
			case "protocol":
				if !errors.Is(f.Err, ErrProtocol) || f.StatusCode != 206 {
					t.Fatal(f)
				}
			case "truncated":
				if !errors.Is(f.Err, io.ErrUnexpectedEOF) || f.Offset != 5 || f.Operation != "page_body" {
					t.Fatal(f)
				}
			}
		})
	}
}

func TestStreamFailureRetainsErrorBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s := &Stream{ctx: ctx, operation: "page_validate", pageOffset: PageSize, offset: PageSize, statusCode: 503}

	err := &HTTPError{StatusCode: 503, Target: "/secret"}
	if !errors.Is(s.fail(err), context.Canceled) {
		t.Fatal("changed cancellation behavior")
	}

	if f := s.Failure(); f.Err != err || f.StatusCode != 503 || f.ContextErr != context.Canceled || strings.Contains(f.Operation, "secret") {
		t.Fatal(f)
	}

	s.fail(io.EOF)

	if s.Failure().Err != err {
		t.Fatal("overwrote first evidence")
	}
}
