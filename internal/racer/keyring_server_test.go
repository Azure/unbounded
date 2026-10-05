// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func keyringRequest(t *testing.T, f *servingFixture, bearer bool, query string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, wire.KeyringPath+query, nil)
	if bearer {
		r.TLS = &tls.ConnectionState{HandshakeComplete: true}
		r.Header.Set("Authorization", "Bearer "+f.token)
	} else {
		r.TLS = f.requestState(t)
	}

	return r
}

func requireKeyringResponse(t *testing.T, w *httptest.ResponseRecorder, status int) []byte {
	t.Helper()

	body := responseBody(t, w.Result(), nil, status)
	if w.Header().Get("Cache-Control") != "no-store" || !w.Flushed {
		t.Fatal("response not flushed or cacheable")
	}

	if len(body) > wire.MaxBundleBytes {
		t.Fatal("unbounded keyring response")
	}

	return body
}

func TestHTTPSKeyringAuthenticationAndEncoding(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	_, bundle, _, material := keyState(t, f.a.Keyring)

	expected, err := wire.EncodeBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	for _, bearer := range []bool{true, false} {
		r, err := http.NewRequestWithContext(f.ctx, http.MethodGet, endpoint+wire.KeyringPath, nil)
		if err != nil {
			t.Fatal(err)
		}

		peer := f.client(t, &f.certificate)
		if bearer {
			peer = f.client(t, nil)
			r.Header.Set("Authorization", "Bearer "+f.token)
		}

		response, err := peer.Do(r)

		body := responseBody(t, response, err, http.StatusOK)
		if !bytes.Equal(body, expected) || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json" {
			t.Fatal("response was not the full committed wire bundle")
		}

		for _, key := range material.Keys {
			if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(key.PrivateKey))) {
				t.Fatal("issuer private key disclosed")
			}
		}
	}
}

func TestKeyringRequestValidation(t *testing.T) {
	f := newServingFixture(t)
	handler := f.a.Server.Handler()

	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"anonymous", func(r *http.Request) { r.TLS = &tls.ConnectionState{HandshakeComplete: true} }, 401},
		{"no TLS", func(r *http.Request) { r.TLS = nil }, 401},
		{"both credentials", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }, 401},
		{"empty auth with certificate", func(r *http.Request) { r.Header["Authorization"] = []string{""} }, 401},
		{"duplicate bearer", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header["Authorization"] = []string{"Bearer " + f.token, "Bearer " + f.token}
		}, 401},
		{"bad bearer", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header.Set("Authorization", "Basic abc")
		}, 401},
		{"unverified certificate", func(r *http.Request) { r.TLS.VerifiedChains = nil }, 401},
		{"body", func(r *http.Request) { r.ContentLength = 1 }, 400},
		{"unknown body length", func(r *http.Request) { r.ContentLength = -1 }, 400},
		{"chunked", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, 400},
		{"encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 400},
		{"empty query", func(r *http.Request) { r.URL.ForceQuery = true }, 400},
		{"post", func(r *http.Request) { r.Method = http.MethodPost }, 400},
		{"head", func(r *http.Request) { r.Method = http.MethodHead }, 400},
		{"slash", func(r *http.Request) { r.URL.Path += "/" }, 400},
		{"escaped path", func(r *http.Request) { r.URL.RawPath = "/v1/%6beyring" }, 400},
		{"header limit", func(r *http.Request) {
			r.Header.Set("X-Large", strings.Repeat("x", f.a.Server.Config.Limits.HeaderBytes))
		}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := keyringRequest(t, f, false, "")
			tc.mutate(r)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			requireKeyringResponse(t, w, tc.status)
		})
	}

	for _, query := range []string{"after=0", "after=999", "after=01", "after=+1", "after=%31", "after=1&after=1", "after=1&x=2", "other=1", "after=", "after=-1", "after=18446744073709551616"} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, false, "?"+query))

			status := 400
			if query == "after=0" || query == "after=999" {
				status = 409
			}

			requireKeyringResponse(t, w, status)
		})
	}
}

func TestKeyringBearerLiveBindings(t *testing.T) {
	for _, scenario := range []string{"token", "audience", "pod", "service account", "daemonset", "node", "API outage"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			status := http.StatusForbidden

			var obj client.Object

			key := client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}

			switch scenario {
			case "token", "audience":
				status = http.StatusUnauthorized
				fixtureDependencies[f.a.authority].Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					review := obj.(*authv1.TokenReview)
					review.Status.Authenticated = scenario == "audience"
					review.Status.Audiences = []string{"wrong-audience"}

					return nil
				}})
			case "pod":
				obj, key.Name = &corev1.Pod{}, "worker-pod"
			case "service account":
				obj = &corev1.ServiceAccount{}
			case "daemonset":
				obj = &appsv1.DaemonSet{}
			case "node":
				obj, key = &corev1.Node{}, client.ObjectKey{Name: "worker"}
			case "API outage":
				status = http.StatusServiceUnavailable
				fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("offline")
				}})
			}

			if obj != nil {
				if err := f.a.Topology.Get(f.ctx, key, obj); err != nil {
					t.Fatal(err)
				}

				if scenario == "node" {
					obj.SetLabels(map[string]string{wire.ExclusionLabel: ""})
				} else {
					obj.SetUID("recreated")
				}

				if err := f.a.Topology.Update(f.ctx, obj); err != nil {
					t.Fatal(err)
				}
			}

			w := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
			requireKeyringResponse(t, w, status)
		})
	}
}

func TestKeyringMTLSNeverReadsAPI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)

		var calls atomic.Int64

		unavailable := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				calls.Add(1)
				return errors.New("offline")
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				calls.Add(1)
				return errors.New("offline")
			},
		})
		fixtureDependencies[f.a.authority].Client, fixtureDependencies[f.a.authority].reader = unavailable, unavailable

		if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
			t.Fatal("outage hidden")
		}

		calls.Store(0)

		handler := f.a.Server.Handler()

		for _, query := range []string{"", "?after=1"} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, false, query))

			want := 200
			if query != "" {
				// Without background confirmation the replica expires during the poll.
				want = 503
			}

			body := requireKeyringResponse(t, w, want)
			if want == 204 && len(body) != 0 {
				t.Fatal("204 body")
			}
		}

		if calls.Load() != 0 {
			t.Fatal("mTLS keyring used API")
		}
	})
}

func TestKeyringPollWakeAndTermination(t *testing.T) {
	for _, bearer := range []bool{false, true} {
		for _, scenario := range []string{"timeout", "rotation", "invalidation", "leader canceled", "request canceled", "expired", "bearer revoked", "retired trust"} {
			t.Run(fmt.Sprintf("bearer=%t/%s", bearer, scenario), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					// Isolate poll termination from the default 30-second freshness gate.
					configureFixtureAge(t, f, time.Minute)

					if scenario == "expired" {
						if bearer {
							_, status, _ := authFixture(t)
							f.token = "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Second).Unix())) + ".signature"
							installReview(t, f.a, status, f.token)
						} else {
							f.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(time.Second) })
						}
					}

					ctx, cancel := context.WithCancel(f.ctx)
					defer cancel()

					r := keyringRequest(t, f, bearer, "?after=1").WithContext(ctx)
					handler := f.a.Server.Handler()
					w := httptest.NewRecorder()
					done := make(chan struct{})

					go func() { defer close(done); handler.ServeHTTP(w, r) }()

					synctest.Wait()

					polls := f.a.Server.keyringPolls.count()

					if polls != 1 || len(f.a.Server.writes) != 0 || len(f.a.Server.bootstrapSlots) != 0 {
						t.Fatal("poll not parked independently of auth/write admission")
					}

					want := 503

					switch scenario {
					case "timeout":
						want = 204
						// Repeated unchanged reconciliations must not extend the wait.
						time.Sleep(20 * time.Second)
						runKeys(t, f.a.Keyring)
						time.Sleep(10 * time.Second)
					case "expired":
						want = 401

						time.Sleep(time.Second)
					case "rotation":
						want = 200
						_, _, rotation, _ := keyState(t, f.a.Keyring)
						fixtureDependencies[f.a.authority].now = func() time.Time { return rotation.NextRotation }
						runKeys(t, f.a.Keyring)
					case "invalidation":
						invalidateFixtureTrust(t, f)
					case "leader canceled":
						f.cancel()
					case "request canceled":
						cancel()
					case "bearer revoked":
						want = 204
						if bearer {
							want = 403
						}

						pod := &corev1.Pod{}
						if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
							t.Fatal(err)
						}

						if err := f.a.Topology.Delete(f.ctx, pod); err != nil {
							t.Fatal(err)
						}

						time.Sleep(wire.PollWait)
					case "retired trust":
						want = 401
						if bearer {
							want = 200
						}

						replaceFixtureCredentials(t, f)
					}

					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("poll did not wake")
					}

					body := requireKeyringResponse(t, w, want)
					if want == 200 {
						bundle, err := wire.DecodeBundle(bytes.NewReader(body))
						if err != nil || bundle.Generation != 2 {
							t.Fatalf("updated bundle: %v", err)
						}
					}

					if f.a.Server.keyringPolls.count() != 0 {
						t.Fatal("admission leaked")
					}
				})
			})
		}
	}
}

func TestKeyringAdmissionHeldThroughResponse(t *testing.T) {
	for _, flush := range []bool{false, true} {
		for _, status := range []int{200, 204, 409} {
			t.Run(fmt.Sprintf("flush=%t/status=%d", flush, status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					f.a.Server.Config.Limits.MaxPolls = 1
					// Keep authority fresh through the 204 wait and blocked response.
					configureFixtureAge(t, f, time.Minute)
					handler := f.a.Server.Handler()

					query := ""
					if status == 204 {
						query = "?after=1"
					}

					if status == 409 {
						query = "?after=2"
					}

					w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: flush}
					// A 204 has no Write, only Flush.
					if status == 204 {
						w.blockFlush = true
					}

					unblock := sync.OnceFunc(func() { close(w.unblock) })
					defer unblock()

					done := make(chan struct{})
					r := keyringRequest(t, f, false, query)

					go func() { defer close(done); handler.ServeHTTP(w, r) }()

					<-w.entered

					other := *f

					other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
					for _, fixture := range []*servingFixture{f, &other} {
						second := httptest.NewRecorder()
						handler.ServeHTTP(second, keyringRequest(t, fixture, false, ""))
						requireKeyringResponse(t, second, 429)
					}
					// Independent snapshot admission is available while delivery blocks.
					if !f.a.Server.admitPoll(wire.NodeID(testNodeUID)) {
						t.Fatal("keyring consumed snapshot admission")
					}

					f.a.Server.releasePoll(wire.NodeID(testNodeUID))
					unblock()
					<-done
					requireKeyringResponse(t, w.ResponseRecorder, status)

					if f.a.Server.keyringPolls.count() != 0 || len(f.a.Server.writes) != 0 {
						t.Fatal("admission leaked")
					}
				})
			})
		}
	}
}

func TestKeyringBearerAdmissionAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		s := f.a.Server
		s.Config.Limits.MaxConcurrentBootstrap = 1
		s.Config.Limits.WriteTimeout = time.Second
		fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		handler := s.Handler()
		w := httptest.NewRecorder()
		r := keyringRequest(t, f, true, "")
		done := make(chan struct{})

		go func() { defer close(done); handler.ServeHTTP(w, r) }()

		synctest.Wait()

		if len(s.bootstrapSlots) != 1 {
			t.Fatal("bearer API work not admitted")
		}

		second := httptest.NewRecorder()
		handler.ServeHTTP(second, keyringRequest(t, f, true, ""))
		requireKeyringResponse(t, second, 429)

		local := httptest.NewRecorder()
		handler.ServeHTTP(local, keyringRequest(t, f, false, ""))
		requireKeyringResponse(t, local, 200)
		time.Sleep(time.Second)
		<-done
		requireKeyringResponse(t, w, 503)

		if len(s.bootstrapSlots) != 0 || s.keyringPolls.count() != 0 {
			t.Fatal("API timeout leaked admission")
		}
	})
}

func TestKeyringPerNodeAdmissionIndependentOfSnapshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		f.a.Server.Config.Limits.MaxPolls = 2
		handler := f.a.Server.Handler()

		ctx, cancel := context.WithCancel(f.ctx)
		defer cancel()

		request := keyringRequest(t, f, false, "?after=1").WithContext(ctx)
		done := make(chan struct{})

		go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), request) }()

		synctest.Wait()

		for _, bearer := range []bool{true, false} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, bearer, ""))
			requireKeyringResponse(t, w, 429)
		}

		other := *f
		other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, keyringRequest(t, &other, false, ""))
		requireKeyringResponse(t, w, 200)
		w = httptest.NewRecorder()
		snapshot := keyringRequest(t, f, false, "")
		snapshot.URL.Path = wire.SnapshotPath
		handler.ServeHTTP(w, snapshot)
		responseBody(t, w.Result(), nil, 200)
		cancel()
		<-done
	})
}

func TestKeyringBearerExpiresDuringReauthentication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		_, status, _ := authFixture(t)
		f.token = "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Second).Unix())) + ".signature"
		installReview(t, f.a, status, f.token)

		reads := 0
		fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				reads++
				if reads == 2 {
					<-ctx.Done()
					return ctx.Err()
				}
			}

			return c.Get(ctx, key, obj, opts...)
		}})
		w := httptest.NewRecorder()
		f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
		requireKeyringResponse(t, w, 401)

		if reads != 2 {
			t.Fatal("did not exercise post-wait expiration")
		}
	})
}
