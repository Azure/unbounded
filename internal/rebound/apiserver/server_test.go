// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	certutil "k8s.io/client-go/util/cert"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
)

func TestServer(t *testing.T) {
	_, kube, _ := fixture(t)

	certPEM, keyPEM, err := certutil.GenerateSelfSignedCertKey("127.0.0.1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := dynamiccertificates.NewStaticCertKeyContent("test", certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { listener.Close() })

	opts := NewOptions()
	opts.SecureServing.Listener = listener

	var denied atomic.Bool

	s, err := opts.NewServer(ServerConfig{
		Client: kube, Namespace: "system", Cert: cert,
		Authenticator: authenticator.RequestFunc(func(*http.Request) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "test-user", Groups: []string{"system:authenticated"}}}, true, nil
		}),
		Authorizer: authorizer.AuthorizerFunc(func(_ context.Context, attr authorizer.Attributes) (authorizer.Decision, string, error) {
			if denied.Load() && attr.IsResourceRequest() {
				return authorizer.DecisionDeny, "test denial", nil
			}

			return authorizer.DecisionAllow, "", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid test certificate")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: time.Second}
	base := "https://" + listener.Addr().String()
	deadline := time.Now().Add(20 * time.Second)

	for {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("server not ready: %v", err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	requestHTTP := func(method, path, accept string, body []byte, code int) []byte {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")

		if accept != "" {
			req.Header.Set("Accept", accept)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != code {
			t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, data)
		}

		return data
	}
	apiPath := "/apis/" + rebound.SchemeGroupVersion.String()
	data := requestHTTP(http.MethodGet, apiPath, "", nil, http.StatusOK)

	var resources metav1.APIResourceList
	if err := json.Unmarshal(data, &resources); err != nil {
		t.Fatal(err)
	}

	if len(resources.APIResources) != 1 {
		t.Fatalf("resources: %s", data)
	}

	r := resources.APIResources[0]
	if r.Name != "nodegrants" || !r.Namespaced || !slices.Contains(r.ShortNames, "ngr") {
		t.Fatalf("resource: %+v", r)
	}

	for _, verb := range []string{"get", "list", "watch", "create", "delete", "deletecollection"} {
		if !slices.Contains(r.Verbs, verb) {
			t.Fatalf("missing verb %s: %+v", verb, r.Verbs)
		}
	}

	if slices.Contains(r.Verbs, "update") || slices.Contains(r.Verbs, "patch") {
		t.Fatalf("mutable verbs: %v", r.Verbs)
	}

	path := apiPath + "/namespaces/team/nodegrants"
	g := grant("http-test", "node-a")
	g.TypeMeta = metav1.TypeMeta{APIVersion: rebound.SchemeGroupVersion.String(), Kind: rebound.Kind}

	data, err = json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}

	data = requestHTTP(http.MethodPost, path, "", data, http.StatusCreated)

	var result rebound.NodeGrant
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	if result.Status.Holder != "test-user" || result.UID == "" {
		t.Fatalf("created: %s", data)
	}

	deadline = time.Now().Add(5 * time.Second)

	for {
		s.storage.mu.Lock()
		rv := s.storage.history[len(s.storage.history)-1].rv
		s.storage.mu.Unlock()

		if rv == result.ResourceVersion {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("informer did not observe create")
		}

		time.Sleep(10 * time.Millisecond)
	}

	data = requestHTTP(http.MethodGet, path+"/http-test", "application/json;as=Table;v=v1;g=meta.k8s.io", nil, http.StatusOK)

	var table metav1.Table
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}

	if table.Kind != "Table" || len(table.Rows) != 1 || len(table.ColumnDefinitions) != 5 {
		t.Fatalf("table: %s", data)
	}

	requestHTTP(http.MethodGet, path+"?fieldSelector=metadata.name%3Dhttp-test", "", nil, http.StatusOK)

	watchCtx, stopWatch := context.WithCancel(t.Context())
	defer stopWatch()

	watchReq, err := http.NewRequestWithContext(watchCtx, http.MethodGet, base+path+"?watch=true&resourceVersion=0", nil)
	if err != nil {
		t.Fatal(err)
	}

	watchResponse, err := client.Do(watchReq)
	if err != nil {
		t.Fatal(err)
	}

	if watchResponse.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(watchResponse.Body)
		watchResponse.Body.Close()
		t.Fatalf("watch status %d: %s (%v)", watchResponse.StatusCode, body, readErr)
	}

	var wireEvent struct {
		Type   string            `json:"type"`
		Object rebound.NodeGrant `json:"object"`
	}
	if err := json.NewDecoder(watchResponse.Body).Decode(&wireEvent); err != nil {
		t.Fatal(err)
	}

	watchResponse.Body.Close()
	stopWatch()

	if wireEvent.Type != "ADDED" || wireEvent.Object.Name != "http-test" || wireEvent.Object.APIVersion != rebound.SchemeGroupVersion.String() {
		t.Fatalf("watch encoding: %+v", wireEvent)
	}

	requestHTTP(http.MethodGet, "/openapi/v2", "", nil, http.StatusOK)
	requestHTTP(http.MethodGet, "/openapi/v3", "", nil, http.StatusOK)
	requestHTTP(http.MethodGet, path+"?watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan", "", nil, http.StatusBadRequest)
	denied.Store(true)
	requestHTTP(http.MethodGet, path, "", nil, http.StatusForbidden)

	for _, health := range []string{"/readyz", "/livez", "/healthz"} {
		requestHTTP(http.MethodGet, health, "", nil, http.StatusOK)
	}
}
