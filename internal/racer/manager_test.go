// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestAssemble(t *testing.T) {
	// Nil Kubernetes dependencies make unintended constructor API calls fail.
	a := Assemble(Config{}, nil, nil)
	if a.Topology.authority != a.Server.authority {
		t.Fatal("topology and HTTP must share the single publication owner")
	}

	if a.Server.authority == nil {
		t.Fatal("bootstrap must have an issuer")
	}

	if a.Keyring.authority != a.Server.authority || a.Topology.authority != a.Server.authority {
		t.Fatal("controllers, issuance, and serving must share trust")
	}

	if a.Keyring.authority == nil || a.Topology.authority != a.Keyring.authority {
		t.Fatal("controllers and issuance must share the catalog gate")
	}

	if a.Server.Lifecycle != a.Lifecycle {
		t.Fatal("serving must share the process readiness gate")
	}

	for _, cfg := range []Config{a.Server.Config, a.Topology.Config, a.Keyring.Config, a.Replication.Config} {
		if cfg.CertificateLifetime != wire.CertificateLifetime || cfg.SnapshotMaxAge != 30*time.Second {
			t.Fatal("composition did not resolve default lifetimes")
		}
	}

	if a.Server.Config.SnapshotMaxAge != a.Topology.Config.SnapshotMaxAge || a.Server.Config.SnapshotMaxAge != a.Keyring.Config.SnapshotMaxAge {
		t.Fatal("freshness owners differ from effective configuration")
	}

	if a.authority.PublicationReady() == nil || a.Server.NeedLeaderElection() || a.Replication.NeedLeaderElection() {
		t.Fatal("missing local accepted state or process-scoped server")
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
		"workload": func() error { _, err := members.DesiredDaemonSet(members.Config{}); return err },
		"server":   func() error { return a.Server.Start(ctx) },
		"run":      func() error { return Run(ctx, Config{}) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, wire.InvalidRequest) {
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
