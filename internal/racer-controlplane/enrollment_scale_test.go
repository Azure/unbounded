// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestEnrollmentRetryWaveBoundsAPIWork(t *testing.T) {
	p, d, n, site := enrollmentObjects()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	var reads atomic.Int32

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p, d, n, site).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	entered := make(chan struct{}, enrollmentConcurrency)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		issued    atomic.Int32
		unbounded atomic.Bool
	)

	server := &enrollmentServer{kube: kube, review: &reviewTestClient{review: validReview}, namespace: "system", issue: func(ctx context.Context, _ string, _ enrollmentIdentity) (enrollmentResponse, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > enrollmentTimeout {
			unbounded.Store(true)
		}

		if issued.Add(1) <= enrollmentConcurrency {
			entered <- struct{}{}

			<-ctx.Done()

			return enrollmentResponse{}, ctx.Err()
		}

		return enrollmentResponse{Certificate: "leaf+chain", Generation: 1}, nil
	}}
	request := func(ctx context.Context) *http.Request {
		req := httptest.NewRequestWithContext(ctx, "POST", "https://control/v3/enroll", strings.NewReader(`{"csr":"node-csr","pod_namespace":"system","pod_name":"racer-worker"}`))
		req.Header.Set("X-Racer-Boot", strings.Repeat("a", 64))
		req.Header.Set("Authorization", "Bearer token")

		return req
	}

	done := make(chan int, enrollmentConcurrency)
	for range enrollmentConcurrency {
		go func() {
			w := httptest.NewRecorder()
			server.enroll(w, request(ctx))

			done <- w.Code
		}()
	}

	for range enrollmentConcurrency {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("admitted enrollments did not reach issuer")
		}
	}

	before := reads.Load()

	for range 1500 {
		w := httptest.NewRecorder()
		server.enroll(w, request(t.Context()))

		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("retry wave request was not shed: %d", w.Code)
		}
	}

	if reads.Load() != before || issued.Load() != enrollmentConcurrency {
		t.Fatalf("retry wave added API or issuance work: reads=%d before=%d issued=%d", reads.Load(), before, issued.Load())
	}

	cancel()

	for range enrollmentConcurrency {
		select {
		case code := <-done:
			if code != http.StatusServiceUnavailable {
				t.Fatalf("canceled issuance returned %d", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("canceled enrollment retained admission slot")
		}
	}

	w := httptest.NewRecorder()
	server.enroll(w, request(t.Context()))

	if w.Code != http.StatusOK || server.inflight.Load() != 0 || unbounded.Load() {
		t.Fatalf("fresh retry did not recover with a bounded deadline: status=%d inflight=%d unbounded=%v", w.Code, server.inflight.Load(), unbounded.Load())
	}
}

func TestEnrollmentFailureReleasesAdmission(t *testing.T) {
	server := new(enrollmentServer)

	for range 2 * enrollmentConcurrency {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "https://control/v3/enroll", strings.NewReader("invalid"))
		server.enroll(w, req)

		if w.Code != http.StatusBadRequest || server.inflight.Load() != 0 {
			t.Fatalf("invalid request retained admission: status=%d inflight=%d", w.Code, server.inflight.Load())
		}
	}
}
