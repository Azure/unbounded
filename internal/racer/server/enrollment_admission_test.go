// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestEnrollmentSaturationPreservesLocalAuthentication(t *testing.T) {
	f := newServingFixture(t)
	s := f.a.Server
	s.Config.Limits.MaxConcurrentBootstrap = 1
	entered := make(chan struct{})
	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			close(entered)
			<-ctx.Done()

			return ctx.Err()
		},
	})
	endpoint := f.start(t)

	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	r := httptest.NewRequestWithContext(ctx, http.MethodPost, wire.BootstrapPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.TLS = &tls.ConnectionState{HandshakeComplete: true}
	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)

		done <- w
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("enrollment did not enter API wait")
	}

	// A fresh TLS connection and snapshot both succeed while enrollment is full.
	peer := f.client(t, &f.certificate)
	response, err := peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusOK)
	// Isolated admission must preserve certificate authentication.
	anonymous := f.client(t, nil)
	response, err = anonymous.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusUnauthorized)
	// New enrollment reaches HTTP backpressure without another API operation.
	response, err = anonymous.Post(endpoint+wire.BootstrapPath, "application/json", bytes.NewReader(body))
	responseBody(t, response, err, http.StatusTooManyRequests)

	if len(s.bootstrapSlots) != 1 || len(s.authSlots) != 0 {
		t.Fatal("enrollment consumed local authentication capacity")
	}

	// Saturation cannot bypass withdrawn trust, even on an established connection.
	fixtureDependencies[f.a.authority].reader = f.a.Topology.Client
	invalidateFixtureTrust(t, f)

	response, err = peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusServiceUnavailable)
	fresh := f.client(t, &f.certificate)

	response, err = fresh.Get(endpoint + wire.SnapshotPath)
	if err == nil {
		response.Body.Close()
		t.Fatal("fresh handshake accepted withdrawn trust")
	}

	cancel()

	select {
	case w := <-done:
		responseBody(t, w.Result(), nil, http.StatusServiceUnavailable)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled enrollment retained admission")
	}

	if len(s.bootstrapSlots) != 0 || len(s.authSlots) != 0 {
		t.Fatal("admission leaked after cancellation")
	}
}
