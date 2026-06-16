// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package examplemodule_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/dashboard/contract"
	"github.com/Azure/unbounded/internal/dashboard/examplemodule"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	examplemodule.New().Routes(mux, "/dashboard/v1")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestManifestCapabilities(t *testing.T) {
	srv := newServer(t)

	var mf contract.Manifest
	getJSON(t, srv.URL+"/dashboard/v1/manifest", &mf)

	if mf.ID != "example" {
		t.Errorf("id = %q", mf.ID)
	}

	for _, c := range []contract.Capability{
		contract.CapabilitySummary, contract.CapabilityResources,
		contract.CapabilityDetails, contract.CapabilityActions, contract.CapabilityStream,
	} {
		if !mf.HasCapability(c) {
			t.Errorf("manifest missing capability %q", c)
		}
	}
}

func TestToggleHealthChangesSummary(t *testing.T) {
	srv := newServer(t)

	var before contract.Summary
	getJSON(t, srv.URL+"/dashboard/v1/summary", &before)

	// Seeded state: charlie is unhealthy -> warning.
	if before.Health != contract.HealthWarning {
		t.Fatalf("initial health = %q, want warning", before.Health)
	}

	// Heal charlie; summary should become OK.
	post(t, srv.URL+"/dashboard/v1/actions/toggle-health", "name=charlie")

	var after contract.Summary
	getJSON(t, srv.URL+"/dashboard/v1/summary", &after)

	if after.Health != contract.HealthOK {
		t.Errorf("after healing, health = %q, want ok", after.Health)
	}
}

func TestWidgetDetailNotFound(t *testing.T) {
	srv := newServer(t)

	resp, err := http.Get(srv.URL + "/dashboard/v1/resources/widgets/missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d", url, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func post(t *testing.T, url, body string) {
	t.Helper()

	resp, err := http.Post(url, "application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s: status %d", url, resp.StatusCode)
	}
}
