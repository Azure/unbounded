// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dhcp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestHTTPDecisionProviderAuthenticatesEndpointRequest(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/netboot/endpoints/edge-1/dhcp/aa:bb:cc:dd:ee:ff" {
			t.Errorf("path = %q", got)
		}
		if got := r.URL.Query().Get("httpClient"); got != "true" {
			t.Errorf("httpClient = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer edge-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"mac":"aa:bb:cc:dd:ee:ff","ipv4":"10.0.1.20","subnetMask":"255.255.255.0"},"transport":"HTTP","bootFile":"https://boot.example/shim.efi"}`))
	}))
	defer backend.Close()

	provider, err := NewHTTPDecisionProvider(backend.URL, "edge-1", "edge-token", backend.Client())
	if err != nil {
		t.Fatal(err)
	}
	decision, err := provider.Decide(t.Context(), "aa:bb:cc:dd:ee:ff", true)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Transport != v1alpha3.NetbootTransportHTTP || decision.Lease.IPv4 != "10.0.1.20" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestHTTPDecisionProviderTreatsMissingSessionAsNoDecision(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()

	provider, err := NewHTTPDecisionProvider(backend.URL, "edge-1", "edge-token", backend.Client())
	if err != nil {
		t.Fatal(err)
	}
	decision, err := provider.Decide(t.Context(), "aa:bb:cc:dd:ee:ff", false)
	if err != nil {
		t.Fatal(err)
	}
	if decision != nil {
		t.Fatalf("decision = %#v, want nil", decision)
	}
}

func TestHTTPDecisionProviderReloadsProjectedToken(t *testing.T) {
	t.Parallel()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantToken := "first"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want token %q", got, wantToken)
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()

	provider, err := NewHTTPDecisionProviderFromTokenFile(backend.URL, "edge-1", tokenFile, backend.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Decide(t.Context(), "aa:bb:cc:dd:ee:ff", false); err != nil {
		t.Fatal(err)
	}
	wantToken = "second"
	if err := os.WriteFile(tokenFile, []byte(wantToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Decide(t.Context(), "aa:bb:cc:dd:ee:ff", false); err != nil {
		t.Fatal(err)
	}
}
