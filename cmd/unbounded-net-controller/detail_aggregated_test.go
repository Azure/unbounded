// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
	"github.com/Azure/unbounded/internal/net/webhook"
)

func TestAggregatedDetailsUsesTrustedProxyAndSharedLifecycle(t *testing.T) {
	certPEM, _, caPEM, err := webhook.GenerateClientAuthCertificateForTest("front-proxy-client")
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(mustParseCertPEM(t, certPEM))
	if err != nil {
		t.Fatal(err)
	}

	server := testWebhookServerForPush(t, caPEM)
	manager := testDetailRequests(t, nodeDetailRequestHooks{})
	health := &healthState{detailRequests: manager, registerAggregatedAPIServer: true}
	health.isLeader.Store(true)

	mux := http.NewServeMux()
	registerStatusHandlers(mux, health, true, server, nil, nil)

	path := "/apis/status.net.unbounded-cloud.io/v1alpha1/nodes/node/details"

	send := func(method, path, body string, trusted bool) *httptest.ResponseRecorder {
		t.Helper()

		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("X-Remote-User", "viewer")

		if trusted {
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		}

		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)

		return recorder
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if response := send(method, path, "{}", false); response.Code != http.StatusForbidden {
			t.Fatalf("spoofed front-proxy header accepted: %d", response.Code)
		}
	}

	response := send(http.MethodPost, path, `{"forceRefresh":true}`, true)
	if response.Code != http.StatusAccepted {
		t.Fatalf("aggregated request failed: %d %s", response.Code, response.Body.String())
	}

	var pending statusv1alpha1.NodeDetailResult
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}

	if err := manager.Complete("node", pending.RequestID, testDetailStatus()); err != nil {
		t.Fatal(err)
	}

	for _, resultPath := range []string{path, "/status/node/node/details"} {
		response = send(http.MethodGet, resultPath+"?requestId="+pending.RequestID, "", true)

		var result statusv1alpha1.NodeDetailResult
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}

		if response.Code != http.StatusOK || result.State != statusv1alpha1.NodeDetailComplete ||
			result.RequestID != pending.RequestID || result.Details == nil || result.Details.Status.NodeInfo.Name != "node" {
			t.Fatalf("paths do not share detail lifecycle: %d %+v", response.Code, result)
		}
	}

	if response = send(http.MethodDelete, path, "", true); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method accepted: %d", response.Code)
	}

	disabledMux := http.NewServeMux()
	registerStatusHandlers(disabledMux, &healthState{}, false, server, nil, nil)

	disabled := httptest.NewRecorder()
	disabledMux.ServeHTTP(disabled, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))

	if disabled.Code != http.StatusNotFound {
		t.Fatalf("aggregated route exposed when disabled: %d", disabled.Code)
	}
}
