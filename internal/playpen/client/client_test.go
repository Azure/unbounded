// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestAllocateGeneratesIdempotencyKeyAndSendsPublicKeyToAggregatedAPI(t *testing.T) {
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	var gotKey, gotPublicKey, gotArchitecture string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != allocsPath {
			t.Fatalf("path = %s", r.URL.Path)
		}

		gotKey = r.Header.Get(idempotencyKeyHeader)

		var req operator.AllocRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}

		gotPublicKey = req.WireGuardPublicKey
		gotArchitecture = req.Architecture

		writeJSON(t, w, testAllocResponse())
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.Allocate(t.Context(), AllocateOptions{Architecture: operator.ArchitectureARM64, WireGuardPrivateKey: privateKey.String()})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if gotKey == "" {
		t.Fatal("idempotency key was not sent")
	}

	if gotPublicKey != privateKey.PublicKey().String() {
		t.Fatalf("public key = %q, want %q", gotPublicKey, privateKey.PublicKey().String())
	}

	if gotArchitecture != operator.ArchitectureARM64 {
		t.Fatalf("architecture = %q, want %q", gotArchitecture, operator.ArchitectureARM64)
	}

	if p.Metadata.Pod.Name != "runner-1" {
		t.Fatalf("pod name = %q", p.Metadata.Pod.Name)
	}
}

func TestNewRequiresRESTConfig(t *testing.T) {
	_, err := New(Config{})
	if err == nil || !strings.Contains(err.Error(), "REST config") {
		t.Fatalf("error = %v, want REST config requirement", err)
	}

	_, err = New(Config{RESTConfig: &rest.Config{}})
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("error = %v, want host requirement", err)
	}
}

func TestNewUsesKubernetesAuthTransport(t *testing.T) {
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL, BearerToken: "test-token"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.deallocate(t.Context(), "alloc-key"); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer test-token" {
		t.Fatalf("authorization header = %q, want bearer token", gotAuth)
	}
}

func TestCloseTearsDownTunnelBeforeDeallocateAndIsIdempotent(t *testing.T) {
	fake := &fakeCommander{}

	var deallocCount int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case allocsPath:
			writeJSON(t, w, testAllocResponse())
		case deallocsPath:
			deallocCount++

			if len(fake.commands) == 0 || !strings.Contains(fake.commands[len(fake.commands)-1], "ip link delete ppwg") {
				t.Fatalf("dealloc happened before tunnel teardown: %#v", fake.commands)
			}

			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client(), cmd: fake})
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.Allocate(t.Context(), AllocateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := p.ConfigureTunnel(t.Context()); err != nil {
		t.Fatalf("configure tunnel: %v", err)
	}

	if err := p.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := p.Close(t.Context()); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if deallocCount != 1 {
		t.Fatalf("dealloc count = %d, want 1", deallocCount)
	}
}

func TestDeallocateSendsIdempotencyKey(t *testing.T) {
	var gotKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deallocsPath {
			t.Fatalf("path = %s", r.URL.Path)
		}

		gotKey = r.Header.Get(idempotencyKeyHeader)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.deallocate(t.Context(), "alloc-key"); err != nil {
		t.Fatal(err)
	}

	if gotKey != "alloc-key" {
		t.Fatalf("idempotency key = %q", gotKey)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func testAllocResponse() operator.AllocResponse {
	return operator.AllocResponse{
		Pod: operator.PodResponse{
			Namespace:    "playpen",
			Name:         "runner-1",
			NodeName:     "node-1",
			Architecture: operator.ArchitectureAMD64,
		},
		Endpoint: operator.EndpointResponse{
			Host:             "20.30.40.50",
			WireGuardUDPPort: 32000,
		},
		WireGuard: operator.WireGuardResponse{
			ServerPublicKey: testPublicKey(),
			ServerAddress:   "10.88.0.1/24",
			ClientAddress:   "10.88.0.2/32",
		},
		VXLAN: operator.VXLANResponse{
			VNI:     12001,
			UDPPort: 4789,
		},
		Redfish: map[string]string{
			"url":                    "https://10.88.0.1:8443",
			"username":               "admin",
			"password":               "secret",
			"serialConsoleStreamURI": "/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream",
		},
	}
}

func testPublicKey() string {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		panic(err)
	}

	return key.PublicKey().String()
}

type fakeCommander struct {
	commands []string
}

func (f *fakeCommander) Run(_ context.Context, name string, args ...string) error {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))

	return nil
}
