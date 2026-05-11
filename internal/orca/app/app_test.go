// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"net/http"
	"net/http/httptest"
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

// TestApp_IsReady covers the AND logic over the two readiness
// preconditions: cachestore-ready AND cluster-has-initial-snapshot.
// Both must be true for isReady to return true.
//
// We can't construct *cluster.Cluster directly here (peers is a
// private atomic.Pointer), so this test goes through isReady's
// observable behaviour by checking the cachestoreReady gate.
// The HasInitialSnapshot path is covered indirectly by the
// integration suite which exercises full bootstrap.
func TestApp_IsReady_RequiresCachestoreReady(t *testing.T) {
	t.Parallel()
	// Building a real *cluster.Cluster here would tie this test to
	// cluster.New's package-internal behaviour. Instead we exercise
	// the gate at the App.isReady level via the underlying boolean
	// composition: when cachestoreReady is false, isReady must be
	// false irrespective of the cluster state.
	//
	// The cluster.HasInitialSnapshot() side is exercised by the
	// orca-inttest suite which drives the full bootstrap path.
	a := &App{cachestoreReady: false}
	// Cluster left nil; calling isReady on it would panic if the
	// gate were not short-circuiting on cachestoreReady. Failure
	// to short-circuit is the regression we want to catch.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("isReady panicked instead of short-circuiting on cachestoreReady=false: %v", r)
		}
	}()

	if a.isReady() {
		t.Errorf("isReady = true with cachestoreReady=false")
	}
}
