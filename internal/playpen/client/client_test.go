// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestAllocateSendsAuthIdempotencyAndPublicKey(t *testing.T) {
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	var gotAuth, gotKey, gotPublicKey string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/playpen/v1/claims" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get(idempotencyKeyHeader)

		var req operator.ClaimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotPublicKey = req.WireGuardPublicKey

		writeJSON(t, w, testClaimResponse())
	}))
	defer server.Close()

	c, err := New(Config{
		BaseURL:                 server.URL,
		CertFingerprint:         tlsServerFingerprint(server),
		GitHubActionsOAuthToken: "github-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.Allocate(t.Context(), AllocateOptions{IdempotencyKey: "claim-key", WireGuardPrivateKey: privateKey.String()})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if gotAuth != "Bearer github-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotKey != "claim-key" {
		t.Fatalf("idempotency key = %q", gotKey)
	}
	if gotPublicKey != privateKey.PublicKey().String() {
		t.Fatalf("public key = %q, want %q", gotPublicKey, privateKey.PublicKey().String())
	}
	if p.Metadata.Pod.Name != "runner-1" {
		t.Fatalf("pod name = %q", p.Metadata.Pod.Name)
	}
}

func TestAllocateRejectsWrongOperatorFingerprint(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, testClaimResponse())
	}))
	defer server.Close()

	c, err := New(Config{
		BaseURL:                 server.URL,
		CertFingerprint:         formatFingerprint(make([]byte, sha256.Size)),
		GitHubActionsOAuthToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Allocate(t.Context(), AllocateOptions{IdempotencyKey: "claim-key"})
	if err == nil || !strings.Contains(err.Error(), "TLS cert SHA256 mismatch") {
		t.Fatalf("allocate error = %v, want TLS mismatch", err)
	}
}

func TestNewRequiresExactlyOneToken(t *testing.T) {
	_, err := New(Config{BaseURL: "https://example.com", CertFingerprint: "fp"})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing token error = %v", err)
	}

	_, err = New(Config{BaseURL: "https://example.com", CertFingerprint: "fp", GitHubActionsOAuthToken: "a", KubernetesToken: "b"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("two token error = %v", err)
	}
}

func TestCloseTearsDownTunnelBeforeReleaseAndIsIdempotent(t *testing.T) {
	fake := &fakeCommander{}
	var releaseCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playpen/v1/claims":
			writeJSON(t, w, testClaimResponse())
		case "/playpen/v1/releases":
			releaseCount++
			if len(fake.commands) == 0 || !strings.Contains(fake.commands[len(fake.commands)-1], "ip link delete ppwg") {
				t.Fatalf("release happened before tunnel teardown: %#v", fake.commands)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client(), KubernetesToken: "k8s-token", Commander: fake})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Allocate(t.Context(), AllocateOptions{IdempotencyKey: "claim-key"})
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
	if releaseCount != 1 {
		t.Fatalf("release count = %d, want 1", releaseCount)
	}
}

func TestReleaseSendsIdempotencyKey(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(idempotencyKeyHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client(), KubernetesToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(t.Context(), "claim-key"); err != nil {
		t.Fatal(err)
	}
	if gotKey != "claim-key" {
		t.Fatalf("idempotency key = %q", gotKey)
	}
}

func tlsServerFingerprint(server *httptest.Server) string {
	cert := server.TLS.Certificates[0].Certificate[0]
	sum := sha256.Sum256(cert)

	return formatFingerprint(sum[:])
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func testClaimResponse() operator.ClaimResponse {
	return operator.ClaimResponse{
		Pod: operator.PodResponse{
			Namespace: "playpen",
			Name:      "runner-1",
			NodeName:  "node-1",
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

func TestHTTPClientDoesNotRequireFingerprint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, testClaimResponse())
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client(), KubernetesToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.Allocate(ctx, AllocateOptions{IdempotencyKey: "claim-key"}); err != nil {
		t.Fatal(err)
	}
}
