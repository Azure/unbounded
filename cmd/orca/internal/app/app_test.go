// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOpsHandler_Healthz_AlwaysReturnsOK locks the contract that
// /healthz is process-liveness only: it returns 200 unconditionally,
// without consulting any readiness signal. Kubelet liveness probes
// must succeed even before the app has fully bootstrapped.
func TestOpsHandler_Healthz_AlwaysReturnsOK(t *testing.T) {
	t.Parallel()

	// readyFn is set to always-false; healthz must still 200.
	h := newOpsHandler(func() bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// TestOpsHandler_Readyz_NotReadyReturns503 verifies that /readyz
// surfaces 503 Service Unavailable while the readiness signal is
// false. Kubelet readiness probes use 503 to gate Service endpoint
// inclusion so traffic does not arrive until the app is ready.
func TestOpsHandler_Readyz_NotReadyReturns503(t *testing.T) {
	t.Parallel()

	h := newOpsHandler(func() bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// TestOpsHandler_Readyz_ReadyReturns200 verifies the readiness
// transition from 503 to 200 when the injected signal flips. This
// is the bootstrap path the app drives once the cachestore
// self-test has passed and the cluster has loaded its initial
// peer-set snapshot.
func TestOpsHandler_Readyz_ReadyReturns200(t *testing.T) {
	t.Parallel()

	var ready atomic.Bool

	h := newOpsHandler(ready.Load)

	// Initial: not ready.
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-ready readyz = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	// Flip readiness and re-probe.
	ready.Store(true)

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("post-ready readyz = %d, want %d", rr.Code, http.StatusOK)
	}
}

// TestApp_IsReady_RequiresCachestoreReady locks the AND-gating
// behavior of isReady. When cachestoreReady is false, isReady must
// short-circuit and return false without touching the Cluster
// pointer. Without that short-circuit a self-test failure that
// leaves Cluster nil would panic the /readyz handler.
func TestApp_IsReady_RequiresCachestoreReady(t *testing.T) {
	t.Parallel()

	a := &App{cachestoreReady: false}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("isReady panicked instead of short-circuiting on cachestoreReady=false: %v", r)
		}
	}()

	if a.isReady() {
		t.Errorf("isReady = true with cachestoreReady=false")
	}
}

// TestApp_Wait_DrainsErrChOnCtxCancel verifies that listener errors
// arriving alongside a shutdown ctx are all logged rather than only
// the first being preserved. Pre-fills errCh with three errors,
// then cancels ctx; Wait should drain all three to the logger.
//
// Regression for M-4 / the earlier app.Wait drain work; the
// expanded drain helper now applies to both Wait return paths so a
// multi-listener crash within a tick doesn't lose errors.
func TestApp_Wait_DrainsErrChOnCtxCancel(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	a := &App{
		log:   log,
		errCh: make(chan error, 4),
	}

	a.errCh <- errors.New("edge boom")

	a.errCh <- errors.New("internal boom")

	a.errCh <- errors.New("ops boom")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx already canceled when Wait starts

	if err := a.Wait(ctx); err != nil {
		t.Errorf("Wait err = %v, want nil (ctx canceled)", err)
	}

	out := buf.String()
	for _, want := range []string{"edge boom", "internal boom", "ops boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("drained log missing %q; got %q", want, out)
		}
	}
}
