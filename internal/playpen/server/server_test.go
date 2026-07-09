// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/playpen/kubevirt"
	"github.com/Azure/unbounded/internal/playpen/labels"
)

func TestRedfishSessionAuthenticatesToken(t *testing.T) {
	ctx := context.Background()
	vmMgr := kubevirt.NewManager(newFakeClient(t, redfishSecret("pp-a", "secret")), kubevirt.Config{Namespace: "default"})
	s := &Server{vm: vmMgr, redfishSessions: make(map[string]string)}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/redfish/v1/SessionService/Sessions", bytes.NewBufferString(`{"UserName":"playpen","Password":"secret"}`))
	w := httptest.NewRecorder()
	s.handleRedfishSessionCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("session status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	token := w.Header().Get("X-Auth-Token")
	if token == "" {
		t.Fatalf("missing X-Auth-Token header")
	}

	authReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/redfish/v1/Systems/pp-a", nil)
	authReq.Header.Set("X-Auth-Token", token)
	if _, ok := s.authorizedRedfish(httptest.NewRecorder(), authReq, "pp-a"); !ok {
		t.Fatalf("session token did not authorize allocation")
	}
	if _, ok := s.authorizedRedfish(httptest.NewRecorder(), authReq, "pp-b"); ok {
		t.Fatalf("session token authorized a different allocation")
	}
}

func TestRedfishSystemsRequiresAuthAndListsOnlyAuthenticatedAllocation(t *testing.T) {
	ctx := context.Background()
	vmMgr := kubevirt.NewManager(newFakeClient(t, redfishSecret("pp-a", "secret-a"), redfishSecret("pp-b", "secret-b")), kubevirt.Config{Namespace: "default"})
	s := &Server{vm: vmMgr, redfishSessions: make(map[string]string)}

	unauthReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/redfish/v1/Systems", nil)
	unauth := httptest.NewRecorder()
	s.handleRedfishSystems(unauth, unauthReq)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated systems status = %d, want %d", unauth.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/redfish/v1/Systems", nil)
	req.SetBasicAuth("playpen", "secret-b")
	w := httptest.NewRecorder()
	s.handleRedfishSystems(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("systems status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body struct {
		Members []map[string]string `json:"Members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode systems response: %v", err)
	}
	if len(body.Members) != 1 || body.Members[0]["@odata.id"] != "/redfish/v1/Systems/pp-b" {
		t.Fatalf("systems members = %#v, want only pp-b", body.Members)
	}
}

func TestRedfishSessionDeleteRequiresMatchingTokenHeader(t *testing.T) {
	s := &Server{redfishSessions: map[string]string{"token-a": "pp-a"}}

	missingHeaderReq := httptest.NewRequest(http.MethodDelete, "/redfish/v1/SessionService/Sessions/token-a", nil)
	missingHeader := httptest.NewRecorder()
	s.handleRedfishSessionDelete(missingHeader, missingHeaderReq, "token-a")
	if missingHeader.Code != http.StatusUnauthorized {
		t.Fatalf("delete without token status = %d, want %d", missingHeader.Code, http.StatusUnauthorized)
	}
	if _, ok := s.redfishSessions["token-a"]; !ok {
		t.Fatalf("session was deleted without matching token header")
	}

	req := httptest.NewRequest(http.MethodDelete, "/redfish/v1/SessionService/Sessions/token-a", nil)
	req.Header.Set("X-Auth-Token", "token-a")
	w := httptest.NewRecorder()
	s.handleRedfishSessionDelete(w, req, "token-a")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if _, ok := s.redfishSessions["token-a"]; ok {
		t.Fatalf("session still exists after authorized delete")
	}
}

func redfishSecret(allocationID, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      allocationID + "-redfish",
			Namespace: "default",
			Labels: map[string]string{
				labels.OwnedLabel:        "true",
				labels.ComponentLabel:    "redfish-secret",
				labels.AllocationIDLabel: allocationID,
			},
		},
		Data: map[string][]byte{"username": []byte("playpen"), "password": []byte(password)},
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	return ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
