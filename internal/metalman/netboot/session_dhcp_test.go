// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/dhcp"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

func TestSessionDHCPDecisionUsesReadyImmutableSession(t *testing.T) {
	t.Parallel()

	session := readyDHCPSession()
	server := newSessionDHCPTestServer(t, session)
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff?httpClient=true", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	var decision dhcp.Decision
	if err := json.NewDecoder(response.Body).Decode(&decision); err != nil {
		t.Fatal(err)
	}

	if decision.Lease.IPv4 != "10.0.1.20" {
		t.Errorf("lease IP = %q", decision.Lease.IPv4)
	}

	if decision.Transport != v1alpha3.NetbootTransportHTTP {
		t.Errorf("transport = %q", decision.Transport)
	}

	wantPrefix := "https://boot.example/v1/netboot/sessions/session-1/"
	if len(decision.BootFile) <= len(wantPrefix) || decision.BootFile[:len(wantPrefix)] != wantPrefix {
		t.Errorf("boot file = %q, want prefix %q", decision.BootFile, wantPrefix)
	}
}

func TestSessionDHCPDecisionRequiresEdgeAuthentication(t *testing.T) {
	t.Parallel()

	server := newSessionDHCPTestServer(t, readyDHCPSession())
	server.EdgeAuthenticator = headerAuthenticator("Bearer edge-token")
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff?httpClient=true", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff?httpClient=true", nil)
	request.Header.Set("Authorization", "Bearer edge-token")

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestSessionDHCPDecisionRejectsAmbiguousReadySessions(t *testing.T) {
	t.Parallel()

	first := readyDHCPSession()
	second := first.DeepCopy()
	second.Name = "session-2"
	second.UID = types.UID("session-uid-2")
	server := newSessionDHCPTestServer(t, first, second)
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff?httpClient=true", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestSessionDHCPDecisionOmitsBootFileForRedfishConfiguration(t *testing.T) {
	t.Parallel()

	session := readyDHCPSession()
	session.Spec.Boot.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	server := newSessionDHCPTestServer(t, session)
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff?httpClient=true", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}

	var decision dhcp.Decision
	if err := json.NewDecoder(response.Body).Decode(&decision); err != nil {
		t.Fatal(err)
	}

	if decision.BootFile != "" {
		t.Errorf("boot file = %q, want empty", decision.BootFile)
	}
}

func TestSessionDHCPDecisionIgnoresExpiredReadySession(t *testing.T) {
	t.Parallel()

	session := readyDHCPSession()
	session.Spec.ExpiresAt = metav1.NewTime(time.Unix(999, 0))
	server := newSessionDHCPTestServer(t, session)
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/endpoints/edge/dhcp/aa:bb:cc:dd:ee:ff", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func newSessionDHCPTestServer(t *testing.T, sessions ...*v1alpha3.NetbootSession) *netboot.SessionHTTPServer {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	objects := make([]runtime.Object, len(sessions))
	for i := range sessions {
		objects[i] = sessions[i]
	}

	clientObjects := make([]runtime.Object, 0, len(objects))
	clientObjects = append(clientObjects, objects...)
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(clientObjects...).Build()

	signer, err := netboot.NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test", func() time.Time {
		return time.Unix(1000, 0)
	})
	if err != nil {
		t.Fatal(err)
	}

	return &netboot.SessionHTTPServer{Client: client, Capabilities: signer}
}

func readyDHCPSession() *v1alpha3.NetbootSession {
	return &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{Name: "session-1", UID: types.UID("session-uid-1")},
		Spec: v1alpha3.NetbootSessionSpec{
			Endpoint: v1alpha3.NetbootSessionEndpointSnapshot{Name: "edge", ExternalURL: "https://boot.example"},
			Boot: v1alpha3.NetbootSessionBoot{
				Transport:           v1alpha3.NetbootTransportHTTP,
				ConfigurationSource: v1alpha3.NetbootConfigurationSourceDHCP,
				FirmwareArtifact:    "shimx64.efi",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:ff",
					IPv4:       "10.0.1.20",
					SubnetMask: "255.255.255.0",
				}},
			},
			Artifacts: v1alpha3.NetbootSessionArtifacts{Files: []v1alpha3.NetbootSessionArtifact{{Name: "shimx64.efi"}}},
			ExpiresAt: metav1.NewTime(time.Unix(2000, 0)),
		},
		Status: v1alpha3.NetbootSessionStatus{Phase: v1alpha3.NetbootSessionPhaseReady},
	}
}

type headerAuthenticator string

func (h headerAuthenticator) Authenticate(_ context.Context, request *http.Request) bool {
	return request.Header.Get("Authorization") == string(h)
}
