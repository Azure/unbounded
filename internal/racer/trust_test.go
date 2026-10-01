// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLocalSnapshotsDuringAPIOutage(t *testing.T) {
	f := newServingFixture(t)

	var calls atomic.Int64

	unavailable := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
	})
	f.a.Topology.APIReader = unavailable
	f.a.Keyring.APIReader = unavailable
	f.a.Server.Bootstrap.APIReader = unavailable
	f.a.Server.Bootstrap.Client = unavailable

	f.a.Server.Bootstrap.Issuer.APIReader = unavailable
	if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
		t.Fatal("reconciliation hid API failure")
	}

	if _, err := f.a.Topology.Reconcile(f.ctx, ctrl.Request{}); err == nil {
		t.Fatal("topology hid API failure")
	}

	calls.Store(0)

	endpoint := f.start(t)
	peer := f.client(t, &f.certificate)

	peer.Timeout = wire.PollWait + 5*time.Second
	for range 2 {
		response, err := peer.Get(endpoint + wire.SnapshotPath)
		responseBody(t, response, err, http.StatusOK)
	}

	current, err := f.a.Server.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	response, err := peer.Get(fmt.Sprintf("%s%s?after=%d", endpoint, wire.SnapshotPath, current.record.Sequence))
	responseBody(t, response, err, http.StatusServiceUnavailable)

	if calls.Load() != 0 {
		t.Fatalf("handshake/warm/204 used API: %d", calls.Load())
	}
	// Issuance still needs live authorization during the same outage.
	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, endpoint+wire.BootstrapPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err = peer.Do(req)
	responseBody(t, response, err, http.StatusServiceUnavailable)

	if calls.Load() != 0 {
		t.Fatal("stale replica attempted enrollment authorization")
	}

	f.cancel()

	response, err = peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusServiceUnavailable)
}

func TestObservedInvalidTrustCannotRecoverFromReadFailure(t *testing.T) {
	for _, observer := range []string{"keyring", "topology", "issuance"} {
		t.Run(observer, func(t *testing.T) {
			f := newServingFixture(t)
			endpoint := f.start(t)
			peer := f.client(t, &f.certificate)
			response, err := peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusOK)

			shared := &corev1.Secret{}

			key := client.ObjectKey{Namespace: f.a.Keyring.Config.Namespace, Name: f.a.Keyring.Config.KeyringSecretName}
			if err := f.a.Topology.Get(f.ctx, key, shared); err != nil {
				t.Fatal(err)
			}

			valid := bytes.Clone(shared.Data["bundle.json"])

			shared.Data["bundle.json"] = []byte(`{}`)
			if err := f.a.Topology.Update(f.ctx, shared); err != nil {
				t.Fatal(err)
			}

			switch observer {
			case "keyring":
				_, err = f.a.Keyring.Reconcile(f.ctx, ctrl.Request{})
			case "issuance":
				_, err = f.a.Server.Bootstrap.Issuer.Issue(f.ctx, NodeIdentity{cluster: f.request.Cluster, node: wire.NodeID(testNodeUID), expires: time.Now().Add(time.Hour)}, f.request)
			default:
				_, err = f.a.Topology.Reconcile(f.ctx, ctrl.Request{})
			}

			if err == nil {
				t.Fatal("invalid trust accepted")
			}

			live := f.a.Keyring.APIReader

			f.a.Keyring.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("API offline after invalid observation")
			}})
			if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
				t.Fatal("API failure hidden")
			}

			response, err = peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusServiceUnavailable)

			if _, err := f.a.Server.Trust.pool(); err == nil {
				t.Fatal("read failure restored invalidated roots")
			}

			fresh := f.client(t, &f.certificate)

			response, err = fresh.Get(endpoint + wire.SnapshotPath)
			if err == nil {
				response.Body.Close()
				t.Fatal("handshake accepted missing local trust")
			}

			f.a.Keyring.APIReader = live

			shared.Data["bundle.json"] = valid
			if err := f.a.Topology.Update(f.ctx, shared); err != nil {
				t.Fatal(err)
			}

			runKeys(t, f.a.Keyring)
			reconcileTopology(t, f.a.Topology, f.ctx)

			response, err = peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusOK)
		})
	}
}

func TestTrustReadOutageAtEachAuthorityRead(t *testing.T) {
	for _, resource := range []string{"racer-installation", "racer-version", "racer-issuer", "racer-keyring"} {
		t.Run(resource, func(t *testing.T) {
			f := newServingFixture(t)
			reads := 0

			f.a.Keyring.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == resource {
					reads++
					return errors.New("API offline")
				}

				return c.Get(ctx, key, obj, opts...)
			}})
			if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil || reads != 1 {
				t.Fatalf("expected read outage at %s: %v, reads=%d", resource, err, reads)
			}

			if err := f.a.Server.Ready(nil); err != nil {
				t.Fatalf("read outage withdrew local state: %v", err)
			}
		})
	}
}

func TestTrustRequiresFreshPostReconcileCredentials(t *testing.T) {
	for _, resource := range []string{"racer-installation", "racer-version", "racer-issuer", "racer-keyring"} {
		for _, failure := range []string{"outage", "deleted", "malformed"} {
			t.Run(resource+"/"+failure, func(t *testing.T) {
				r, now := testKeyring(t)
				runKeys(t, r)
				_, _, initial, _ := keyState(t, r)
				*now = initial.NextRotation

				accepted, err := r.Trust.pool()
				if err != nil {
					t.Fatal(err)
				}

				acceptedBundle, _, err := r.Trust.keyring()
				if err != nil || acceptedBundle.generation != 1 {
					t.Fatalf("initial delivery state: %v", err)
				}

				reads := 0
				r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == resource {
						reads++
						if reads == 2 {
							switch failure {
							case "outage":
								return errors.New("post-reconcile API outage")
							case "deleted":
								return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
							case "malformed":
								if err := c.Get(ctx, key, obj, opts...); err != nil {
									return err
								}

								switch value := obj.(type) {
								case *corev1.Secret:
									value.Data = nil
								case *corev1.ConfigMap:
									value.Data = nil
								}

								return nil
							}
						}
					}

					return c.Get(ctx, key, obj, opts...)
				}})

				if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err == nil || reads != 2 {
					t.Fatalf("post-reconcile failure bypassed: %v, reads=%d", err, reads)
				}

				current, err := r.Trust.pool()
				if failure == "outage" {
					if err != nil || current != accepted || !r.Lifecycle.issuer {
						t.Fatalf("read outage replaced accepted trust with candidate roots: %v", err)
					}
				} else if err == nil || r.Lifecycle.issuer {
					t.Fatal("observed invalid authority retained or installed trust")
				}

				currentBundle, _, bundleErr := r.Trust.keyring()
				if failure == "outage" {
					if bundleErr != nil || currentBundle != acceptedBundle {
						t.Fatalf("read outage exposed candidate delivery state: %v", bundleErr)
					}
				} else if bundleErr == nil {
					t.Fatal("observed invalid authority retained delivery state")
				}

				r.APIReader = r.Client

				_, staged, _, _ := keyState(t, r)
				if staged.Generation != 2 || len(staged.PeerTrustRoots) != 2 {
					t.Fatal("failure preceded successful rotation publication")
				}

				runKeys(t, r)

				current, err = r.Trust.pool()
				if err != nil || current.Equal(accepted) {
					t.Fatalf("fresh successful reconciliation did not install staged trust: %v", err)
				}

				currentBundle, _, bundleErr = r.Trust.keyring()
				if bundleErr != nil || currentBundle.generation != 2 {
					t.Fatalf("committed delivery not installed with trust: %v", bundleErr)
				}
			})
		}
	}
}

func TestKeyringCancellationOverridesPostReconcileReadFailure(t *testing.T) {
	for _, failure := range []string{"outage", "conflict", "success"} {
		t.Run(failure, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reads := 0
			r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == r.Config.KeyringSecretName {
					reads++
					if reads == 2 {
						defer cancel()

						switch failure {
						case "outage":
							return errors.New("post-reconcile API outage")
						case "conflict":
							return apierrors.NewConflict(corev1.Resource("secrets"), key.Name, wire.Conflict)
						}
					}
				}

				return c.Get(ctx, key, obj, opts...)
			}})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if reads != 2 || !errors.Is(err, context.Canceled) || !errors.Is(err, reconcile.TerminalError(nil)) || result != (ctrl.Result{}) {
				t.Fatalf("post-reconcile cancellation: reads=%d result=%v err=%v", reads, result, err)
			}

			if _, err := r.Trust.pool(); err == nil || r.Lifecycle.issuer {
				t.Fatal("cancellation after admission retained trust or issuer readiness")
			}

			if _, _, err := r.Trust.keyring(); err == nil {
				t.Fatal("cancellation after admission retained delivery")
			}

			r.APIReader = r.Client

			_, staged, _, _ := keyState(t, r)
			if staged.Generation != 2 || len(staged.PeerTrustRoots) != 2 {
				t.Fatal("cancellation preceded successful rotation publication")
			}
		})
	}
}

func TestReconcilerAlreadyExistsHandling(t *testing.T) {
	for _, operation := range []string{"keyring", "topology"} {
		t.Run(operation, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			accepted, err := r.Trust.pool()
			if err != nil {
				t.Fatal(err)
			}

			writes := 0
			writer := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				writes++
				return apierrors.NewAlreadyExists(corev1.Resource("secrets"), obj.GetName())
			}})

			var result ctrl.Result

			if operation == "keyring" {
				r.Client = writer

				result, err = r.Reconcile(t.Context(), ctrl.Request{})
				if err != nil || result.RequeueAfter != retryConflictDelay {
					t.Fatalf("keyring AlreadyExists not requeued: %v %v", result, err)
				}

				if _, err := r.Trust.pool(); err == nil || r.Lifecycle.issuer {
					t.Fatal("keyring write failure retained trust or issuer readiness")
				}
			} else {
				topology := Assemble(r.Config, writer, r.APIReader).Topology
				topology.Trust = r.Trust

				result, err = topology.Reconcile(t.Context(), ctrl.Request{})
				if !apierrors.IsAlreadyExists(err) || result != (ctrl.Result{}) {
					t.Fatalf("topology AlreadyExists treated as Conflict: %v %v", result, err)
				}

				if current, err := r.Trust.pool(); err != nil || current != accepted {
					t.Fatalf("topology publication write failure changed trust: %v", err)
				}
			}

			if writes != 1 {
				t.Fatalf("expected one failed write, got %d", writes)
			}
		})
	}
}

func TestLocalTrustInvalidationDuringPoll(t *testing.T) {
	f := newServingFixture(t)

	current, err := f.a.Server.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s?after=%d", wire.SnapshotPath, current.record.Sequence), nil)
	req.TLS = f.requestState(t)
	handler := f.a.Server.Handler()
	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		done <- w
	}()

	deadline := time.After(5 * time.Second)

	for {
		n := f.a.Server.polls.count()

		if n == 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("poll did not park")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	f.a.Server.Trust.invalidate()

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	reconcileTopology(t, f.a.Topology, f.ctx)

	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != `{"code":"unavailable"}` {
			t.Fatalf("invalidated trust disclosed publication: %d %s", w.Code, w.Body.String())
		}
	case <-deadline:
		t.Fatal("poll did not recheck local trust")
	}
}

func TestIssuanceTrustObservationLockHonorsDeadline(t *testing.T) {
	f := newServingFixture(t)
	if err := f.a.Keyring.CatalogGate.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer f.a.Keyring.CatalogGate.Release()

	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := f.a.Server.Bootstrap.Issuer.TrustRoots(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gate wait ignored deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("gate wait held enrollment admission past deadline")
	}

	if _, err := f.a.Server.Trust.pool(); err != nil {
		t.Fatalf("canceled gate wait invalidated accepted trust: %v", err)
	}
}

func TestCatalogGateCancellationPreservesAcceptedState(t *testing.T) {
	for _, operation := range []string{"topology", "keyring", "issuance"} {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/held=%t", operation, held), func(t *testing.T) {
				f := newServingFixture(t)

				roots, err := f.a.Server.Trust.pool()
				if err != nil {
					t.Fatal(err)
				}

				publication, err := f.a.Server.Publications.Current()
				if err != nil {
					t.Fatal(err)
				}

				var reads atomic.Int64

				reader := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						reads.Add(1)
						return wire.Unavailable
					},
				})
				f.a.Topology.APIReader = reader
				f.a.Keyring.APIReader = reader
				f.a.Server.Bootstrap.Issuer.APIReader = reader

				gate := f.a.Keyring.CatalogGate
				if held {
					if err := gate.Acquire(t.Context()); err != nil {
						t.Fatal(err)
					}
					defer gate.Release()
				}

				ctx, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
				defer cancel()

				want := context.DeadlineExceeded

				if !held {
					cancel()

					want = context.Canceled
				}

				done := make(chan error, 1)

				go func() {
					var err error

					switch operation {
					case "topology":
						_, err = f.a.Topology.Reconcile(ctx, ctrl.Request{})
					case "keyring":
						_, err = f.a.Keyring.Reconcile(ctx, ctrl.Request{})
					case "issuance":
						_, err = f.a.Server.Bootstrap.Issuer.TrustRoots(ctx)
					}

					done <- err
				}()

				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("gate wait cancellation: %v", err)
					}

					if operation != "issuance" && !errors.Is(err, reconcile.TerminalError(nil)) {
						t.Fatalf("canceled reconcile can retry: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("gate wait ignored cancellation")
				}

				if reads.Load() != 0 {
					t.Fatalf("canceled admission read authority: %d", reads.Load())
				}

				if current, err := f.a.Server.Trust.pool(); err != nil || current != roots {
					t.Fatalf("canceled admission changed accepted trust: %v", err)
				}

				if current, err := f.a.Server.Publications.Current(); err != nil || current != publication {
					t.Fatalf("canceled admission changed publication: %v", err)
				}

				if err := f.a.Server.Ready(nil); err != nil {
					t.Fatalf("canceled admission withdrew readiness: %v", err)
				}
			})
		}
	}
}
