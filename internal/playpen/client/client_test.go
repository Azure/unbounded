// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestAllocateGeneratesIdempotencyKeyAndSendsRequestToAggregatedAPI(t *testing.T) {
	var gotKey, gotArchitecture string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != allocsPath {
			t.Fatalf("path = %s", r.URL.Path)
		}

		gotKey = r.Header.Get(idempotencyKeyHeader)

		var req operator.AllocRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}

		gotArchitecture = req.Architecture

		writeJSON(t, w, testAllocResponse())
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.Allocate(t.Context(), AllocateOptions{Architecture: operator.ArchitectureARM64})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if gotKey == "" {
		t.Fatal("idempotency key was not sent")
	}

	if gotArchitecture != operator.ArchitectureARM64 {
		t.Fatalf("architecture = %q, want %q", gotArchitecture, operator.ArchitectureARM64)
	}

	if p.Metadata.Pod.Name != "runner-1" {
		t.Fatalf("pod name = %q", p.Metadata.Pod.Name)
	}
}

func TestAllocateControlPlaneSendsResourceTypeAndVersion(t *testing.T) {
	var gotKey, gotResourceType, gotVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != allocsPath {
			t.Fatalf("path = %s", r.URL.Path)
		}

		gotKey = r.Header.Get(idempotencyKeyHeader)

		var req operator.AllocRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}

		gotResourceType = req.ResourceType
		gotVersion = req.KubernetesVersion
		writeJSON(t, w, testControlPlaneAllocResponse())
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	cp, err := c.AllocateControlPlane(t.Context(), AllocateOptions{KubernetesVersion: "v1.33.1"})
	if err != nil {
		t.Fatalf("allocate control plane: %v", err)
	}

	if gotKey == "" {
		t.Fatal("idempotency key was not sent")
	}

	if gotResourceType != operator.ResourceTypeControlPlane {
		t.Fatalf("resourceType = %q, want %q", gotResourceType, operator.ResourceTypeControlPlane)
	}

	if gotVersion != "v1.33.1" {
		t.Fatalf("kubernetesVersion = %q, want v1.33.1", gotVersion)
	}

	if cp.Kubeconfig() != "host-kubeconfig" {
		t.Fatalf("kubeconfig = %q", cp.Kubeconfig())
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

			if len(fake.commands) == 0 || !strings.HasPrefix(fake.commands[len(fake.commands)-1], "ip link delete ppvx") {
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

func TestControlPlaneCloseDeallocatesOnce(t *testing.T) {
	var deallocCount int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case allocsPath:
			writeJSON(t, w, testControlPlaneAllocResponse())
		case deallocsPath:
			deallocCount++

			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c, err := New(Config{RESTConfig: &rest.Config{Host: server.URL}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	cp, err := c.AllocateControlPlane(t.Context(), AllocateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := cp.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := cp.Close(t.Context()); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if deallocCount != 1 {
		t.Fatalf("dealloc count = %d, want 1", deallocCount)
	}
}

func TestPlaypenCommandExecutesInCurrentNamespace(t *testing.T) {
	p := &Playpen{
		tunnel: newTunnel(&fakeCommander{}, testAllocResponse(), TunnelConfig{}),
	}

	cmd, err := p.Command(t.Context(), "ip", "addr")
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(cmd.Args, " ")
	if os.Geteuid() == 0 {
		if got != "ip addr" {
			t.Fatalf("command = %q", got)
		}

		return
	}

	if got != "sudo -n ip addr" {
		t.Fatalf("command = %q", got)
	}
}

func TestPlaypenCommandElevatesWrapperCommand(t *testing.T) {
	p := &Playpen{
		tunnel: newTunnel(&fakeCommander{}, testAllocResponse(), TunnelConfig{}),
	}

	cmd, err := p.Command(t.Context(), "env", "FOO=bar", "metalman", "serve-pxe")
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(cmd.Args, " ")
	if os.Geteuid() == 0 {
		if got != "env FOO=bar metalman serve-pxe" {
			t.Fatalf("command = %q", got)
		}

		return
	}

	if got != "sudo -n env FOO=bar metalman serve-pxe" {
		t.Fatalf("command = %q", got)
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
		ResourceType: operator.ResourceTypeRunner,
		Pod: operator.PodResponse{
			Namespace:    "playpen",
			Name:         "runner-1",
			NodeName:     "node-1",
			ResourceType: operator.ResourceTypeRunner,
			Architecture: operator.ArchitectureAMD64,
		},
		Endpoint: operator.EndpointResponse{
			Host: "20.30.40.50",
		},
		VXLAN: operator.VXLANResponse{
			Interface:     "vxlan0",
			Device:        "unbounded0",
			VNI:           12001,
			UDPPort:       4789,
			ServerAddress: "10.88.0.1",
			ClientAddress: "10.88.0.2",
		},
		Network: operator.NetworkResponse{
			GuestMAC:    "52:54:00:aa:bb:01",
			GuestIPv4:   "192.168.200.10",
			SubnetMask:  "255.255.255.0",
			GatewayIPv4: "192.168.200.1",
			DNS:         []string{"8.8.8.8"},
		},
		Redfish: map[string]string{
			"url":                    "https://10.88.0.1:8443",
			"username":               "admin",
			"password":               "secret",
			"serialConsoleStreamURI": "/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream",
		},
	}
}

func testControlPlaneAllocResponse() operator.AllocResponse {
	return operator.AllocResponse{
		ResourceType: operator.ResourceTypeControlPlane,
		Pod: operator.PodResponse{
			Namespace:         "playpen",
			Name:              "control-plane-1",
			NodeName:          "node-1",
			ResourceType:      operator.ResourceTypeControlPlane,
			KubernetesVersion: "v1.33.1",
		},
		Endpoint: operator.EndpointResponse{Host: "20.30.40.50", APIServerTCPPort: 16443},
		ControlPlane: operator.ControlPlaneResponse{
			KubernetesVersion: "v1.33.1",
			Kubeconfig:        "host-kubeconfig",
			APIServerURL:      "https://20.30.40.50:16443",
			GuestAPIServerURL: "https://10.88.0.1:6443",
		},
	}
}

type fakeCommander struct {
	commands []string
}

func (f *fakeCommander) Run(_ context.Context, name string, args ...string) error {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))

	return nil
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if strings.Contains(command, want) {
			return true
		}
	}

	return false
}
