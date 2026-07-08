// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestAllocateVMSendsWireGuardPublicKeyToAggregatedAPI(t *testing.T) {
	var got operator.AllocRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != allocationsPath {
			t.Fatalf("path = %s", r.URL.Path)
		}

		if r.Header.Get(idempotencyKeyHeader) == "" {
			t.Fatalf("missing idempotency key")
		}

		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}

		writeTestJSON(w, operator.AllocResponse{
			ID:           "alloc1",
			Architecture: operator.ArchitectureAMD64,
			Endpoint:     operator.EndpointResponse{Host: "203.0.113.10", WireGuardUDPPort: 51820},
			WireGuard:    operator.WireGuardResponse{ServerPublicKey: "kX4Z6LwejXzAl2m4nA1rY3EWB3yJe2rZXYc2umY7jU0=", ServerAddress: "10.250.1.1/24", ClientAddress: "10.250.1.2/24"},
			VXLAN:        operator.VXLANResponse{VNI: 12001, UDPPort: 4789, ServerAddress: "10.250.1.1", ClientAddress: "10.250.1.2"},
			Network:      operator.NetworkResponse{GuestMAC: "02:00:00:00:00:01", GuestIPv4: "192.168.10.10", GatewayIPv4: "192.168.10.1", SubnetMask: "255.255.255.0"},
		})
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	alloc, err := c.AllocateVM(t.Context(), AllocateVMOptions{Architecture: operator.ArchitectureARM64})
	if err != nil {
		t.Fatal(err)
	}

	if got.Architecture != operator.ArchitectureARM64 {
		t.Fatalf("architecture = %q", got.Architecture)
	}

	if got.WireGuardPublicKey == "" {
		t.Fatalf("missing public key")
	}

	if alloc.Metadata.ID != "alloc1" {
		t.Fatalf("allocation id = %q", alloc.Metadata.ID)
	}
}

func TestCloseTearsDownTunnelBeforeDeallocate(t *testing.T) {
	var deallocated bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case deallocationsPath:
			deallocated = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	fake := &fakeCommander{}
	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client(), cmd: fake})
	if err != nil {
		t.Fatal(err)
	}

	a := &Allocation{
		client:         c,
		idempotencyKey: "key",
		Metadata:       operator.AllocResponse{ID: "alloc1"},
		tunnel:         newTunnel(fake, "private", operator.AllocResponse{}, TunnelConfig{NetworkNamespace: "ns", WireGuardInterface: "wg", ManagementHostInterface: "mh"}),
	}

	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	if !deallocated {
		t.Fatalf("deallocate was not called")
	}

	if len(fake.commands) == 0 {
		t.Fatalf("tunnel teardown was not called")
	}
}

func TestNewRequiresRESTConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value) //nolint:errcheck
}
