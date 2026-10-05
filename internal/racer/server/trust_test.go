// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLocalTrustInvalidationDuringPoll(t *testing.T) {
	f := newServingFixture(t)

	current, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s?after=%d", wire.SnapshotPath, current.Sequence()), nil)
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

	withdrawServerTrust(t, f.a.Server)

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	_, _ = f.a.Topology.Reconcile(f.ctx, ctrl.Request{})

	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != `{"code":"unavailable"}` {
			t.Fatalf("invalidated trust disclosed publication: %d %s", w.Code, w.Body.String())
		}
	case <-deadline:
		t.Fatal("poll did not recheck local trust")
	}
}
