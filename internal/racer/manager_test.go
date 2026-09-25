// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestAssemble(t *testing.T) {
	// Nil Kubernetes dependencies make unintended constructor API calls fail.
	a := Assemble(Config{}, nil, nil)
	if a.Topology.Publications != a.Server.Publications {
		t.Fatal("topology and HTTP must share the single publication owner")
	}

	if a.Keyring.Issuer != a.Server.Bootstrap.Issuer {
		t.Fatal("rotation and issuance must share the issuer")
	}

	if a.Topology.Accepted == nil || !a.Server.NeedLeaderElection() {
		t.Fatal("missing local accepted state or leader-scoped server")
	}

	if err := a.Server.Ready(nil); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("uninitialized server became ready: %v", err)
	}
}

func TestFailClosedEntryPoints(t *testing.T) {
	a := Assemble(Config{}, nil, nil)
	ctx := context.Background()

	operations := map[string]func() error{
		"topology": func() error { _, err := a.Topology.Reconcile(ctx, ctrl.Request{}); return err },
		"keyring":  func() error { _, err := a.Keyring.Reconcile(ctx, ctrl.Request{}); return err },
		"workload": func() error { _, err := a.Workload.Reconcile(ctx, ctrl.Request{}); return err },
		"server":   func() error { return a.Server.Start(ctx) },
		"run":      func() error { return Run(ctx, Config{}) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			want := error(ErrUnimplemented)
			if name == "topology" || name == "run" {
				want = wire.InvalidRequest
			}

			if err := operation(); !errors.Is(err, want) {
				t.Fatalf("entry point did not fail closed: %v", err)
			}
		})
	}
}

func TestScaffoldRoutesCannotAuthenticate(t *testing.T) {
	handler := Assemble(Config{}, nil, nil).Server.Handler()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, wire.BootstrapPath},
		{http.MethodGet, wire.SnapshotPath},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))

			if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"code":"unavailable"}` {
				t.Fatalf("unexpected scaffold response: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSingletonCoalescesObjects(t *testing.T) {
	requests := singleton(context.Background(), nil)
	if len(requests) != 1 || requests[0].Name != "racer" || requests[0].Namespace != "" {
		t.Fatalf("unexpected singleton requests: %v", requests)
	}
}
