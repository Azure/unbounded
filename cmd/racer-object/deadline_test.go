// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// deadlineRecorder records write deadlines set through http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder

	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.deadlines = append(d.deadlines, t)

	return nil
}

func (d *deadlineRecorder) last(t *testing.T) time.Time {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.deadlines) == 0 {
		t.Fatal("write deadline was never set")
	}

	return d.deadlines[len(d.deadlines)-1]
}

// clearingHandler mimics Object.WriteTo, which clears the write deadline
// before returning.
func clearingHandler(t *testing.T, then func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			t.Errorf("clear deadline: %v", err)
		}

		then()
	})
}

func assertRearmed(t *testing.T, got, before, after time.Time, bound time.Duration) {
	t.Helper()

	if got.IsZero() {
		t.Fatal("write deadline left cleared after handler returned")
	}

	if got.Before(before.Add(bound)) || got.After(after.Add(bound)) {
		t.Fatalf("write deadline %v outside [%v, %v]", got, before.Add(bound), after.Add(bound))
	}
}

func TestSidecarHandlerRearmsWriteDeadlineAfterReturn(t *testing.T) {
	const requestTimeout = 2 * time.Second

	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := sidecarHTTPHandler(clearingHandler(t, func() {}), requestTimeout)

	before := time.Now()

	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bucket/key", nil))

	assertRearmed(t, w.last(t), before, time.Now(), requestTimeout+sidecarWriteGrace)
}

func TestSidecarHandlerRearmsWriteDeadlineAfterPanic(t *testing.T) {
	const requestTimeout = 2 * time.Second

	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := sidecarHTTPHandler(clearingHandler(t, func() { panic("boom") }), requestTimeout)

	before := time.Now()

	func() {
		defer func() {
			if value := recover(); !errors.Is(asError(value), http.ErrAbortHandler) {
				t.Errorf("recovered %v, want http.ErrAbortHandler", value)
			}
		}()

		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bucket/key", nil))
	}()

	assertRearmed(t, w.last(t), before, time.Now(), requestTimeout+sidecarWriteGrace)
}

func TestSidecarHandlerIgnoresUnsupportedWriteDeadline(t *testing.T) {
	w := httptest.NewRecorder()
	h := sidecarHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), time.Second)

	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bucket/key", nil))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}
