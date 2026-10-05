// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReplicationRouteAuthorizationAndEarlyListener(t *testing.T) {
	f := newServingFixture(t)
	r := f.a.Replication
	r.Config.ControllerServiceAccount = "racer-controller"
	r.leader = f.ctx
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controller"}}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "racer-controller", UID: "controller-sa"}}
	for _, obj := range []client.Object{pod, sa} {
		if err := r.Client.Create(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	username := "system:serviceaccount:" + r.Config.Namespace + ":racer-controller"
	audience := ReplicationAudience
	r.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review := obj.(*authv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != ReplicationAudience {
			t.Fatal("wrong review audience")
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience}, User: authv1.UserInfo{Username: username, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})
	fixtureDependencies[r.authority].Client = r.Client

	for _, unchanged := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocked flush unchanged=%v failure=%v", unchanged, fail), func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, ReplicationPath, nil)
				request.TLS = f.requestState(t)
				request.Header.Set("Authorization", "Bearer "+f.token)

				want := http.StatusOK

				if unchanged {
					p, err := r.authority.Current()
					if err != nil {
						t.Fatal(err)
					}

					request.URL.RawQuery = fmt.Sprintf("after=%d", p.Sequence())
					want = http.StatusNoContent
				}

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true, fail: fail}

				unblock := sync.OnceFunc(func() { close(w.unblock) })
				defer unblock()

				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				select {
				case <-w.entered:
				case <-time.After(8 * time.Second):
					t.Fatal("explicit flush not reached")
				}

				if len(f.a.Server.writes) != 1 {
					t.Fatal("write admission released before flush")
				}

				held := f.a.Server.replicationPolls.count() == 1

				if !held {
					t.Fatal("poll admission released before flush")
				}

				duplicate := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(duplicate, request.Clone(f.ctx))

				if duplicate.Code != http.StatusTooManyRequests {
					t.Fatalf("duplicate during flush: %d", duplicate.Code)
				}

				unblock()

				var wantAbort any
				if fail {
					wantAbort = http.ErrAbortHandler
				}

				if aborted := <-done; aborted != wantAbort || w.Code != want {
					t.Fatalf("flush result %v status %d", aborted, w.Code)
				}

				if len(f.a.Server.writes) != 0 {
					t.Fatal("write admission leaked after flush")
				}

				held = f.a.Server.replicationPolls.count() != 0

				if held {
					t.Fatal("poll admission leaked after flush")
				}
			})
		}
	}

	for _, tc := range []struct {
		name string
		code int
	}{{"controller", 200}, {"duplicate poll", 429}, {"dataplane", 403}, {"wrong audience", 401}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "duplicate poll" {
				f.a.Server.initializeAdmission()

				if !f.a.Server.replicationPolls.acquire(string(pod.UID)) {
					t.Fatal("could not reserve replication poll")
				}
				defer f.a.Server.replicationPolls.release(string(pod.UID))
			}

			if tc.name == "dataplane" {
				username = "system:serviceaccount:" + r.Config.Namespace + ":racer-dataplane"
			}

			if tc.name == "wrong audience" {
				audience = wire.TokenAudience
			}

			request := httptest.NewRequest(http.MethodGet, ReplicationPath, nil)
			request.TLS = f.requestState(t)
			request.Header.Set("Authorization", "Bearer "+f.token)

			response := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(response, request)

			if response.Code != tc.code {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	// TLS must be reachable before public readiness, including before trust is
	// initialized. Public routes remain unavailable; internal auth is independent.
	invalidateFixtureTrust(t, f)
	endpoint := f.start(t)
	peer := f.client(t, nil)
	response, err := peer.Get(endpoint + ReplicationPath)
	responseBody(t, response, err, http.StatusUnauthorized)
}
