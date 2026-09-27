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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	if body := responseBody(t, response, err, http.StatusNoContent); len(body) != 0 {
		t.Fatal("204 included a body")
	}

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

	if calls.Load() == 0 {
		t.Fatal("enrollment bypassed live authorization")
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
				_, err = f.a.Keyring.Issuer.Issue(f.ctx, NodeIdentity{cluster: f.request.Cluster, node: wire.NodeID(testNodeUID), expires: time.Now().Add(time.Hour)}, f.request)
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
		f.a.Server.Publications.mu.Lock()
		n := len(f.a.Server.Publications.polls)
		f.a.Server.Publications.mu.Unlock()

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
	f.a.Keyring.CatalogMu.Lock()
	defer f.a.Keyring.CatalogMu.Unlock()

	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := f.a.Keyring.Issuer.TrustRoots(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait ignored deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock wait held enrollment admission past deadline")
	}

	if _, err := f.a.Server.Trust.pool(); err != nil {
		t.Fatalf("canceled lock wait invalidated accepted trust: %v", err)
	}
}
