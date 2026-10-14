// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
)

func TestMaxNodesPerGrantOption(t *testing.T) {
	_, kube, _ := fixture(t)
	certPEM, keyPEM, err := certutil.GenerateSelfSignedCertKey("rebound.system.svc", nil, nil)
	require.NoError(t, err)
	cert, err := dynamiccertificates.NewStaticCertKeyContent("test", certPEM, keyPEM)
	require.NoError(t, err)

	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			opts := NewOptions()
			require.Equal(t, 500, opts.MaxNodesPerGrant)

			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			opts.AddFlags(flags)
			require.NoError(t, flags.Parse([]string{"--max-nodes-per-grant=" + value}))

			_, err := opts.NewServer(ServerConfig{Client: kube, Namespace: "system", Cert: cert})
			require.EqualError(t, err, "max-nodes-per-grant must be positive")
		})
	}
}

func TestServerLoopback(t *testing.T) {
	for _, allowAnonymous := range []bool{false, true} {
		name := "reject authentication"
		if allowAnonymous {
			name = "deny authorization"
		}

		t.Run(name, func(t *testing.T) {
			_, kube, _ := fixture(t)

			certPEM, keyPEM, err := certutil.GenerateSelfSignedCertKey("rebound.system.svc", nil, nil)
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

			s, err := opts.NewServer(ServerConfig{
				Client: kube, Namespace: "system", Cert: cert,
				Authenticator: authenticator.RequestFunc(func(*http.Request) (*authenticator.Response, bool, error) {
					if allowAnonymous {
						return &authenticator.Response{User: &user.DefaultInfo{Name: user.Anonymous, Groups: []string{user.AllUnauthenticated}}}, true, nil
					}

					return nil, false, nil
				}),
				Authorizer: authorizer.AuthorizerFunc(func(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
					return authorizer.DecisionDeny, "test denial", nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}

			if s.server.SecureServingInfo.Cert != cert {
				t.Fatal("serving certificate was replaced")
			}

			loopback := rest.CopyConfig(s.server.LoopbackClientConfig)
			if loopback.BearerToken == "" || loopback.ServerName != "apiserver-loopback-client" || loopback.Insecure {
				t.Fatal("loopback requires a bearer token and verified SNI certificate")
			}

			loopback.Timeout = time.Second

			client, err := rest.HTTPClientFor(loopback)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(client.CloseIdleConnections)

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

			deadline := time.Now().Add(20 * time.Second)
			apiURL := loopback.Host + "/apis/" + rebound.SchemeGroupVersion.String()

			for {
				resp, err := client.Get(apiURL)
				if err == nil {
					resp.Body.Close()

					if resp.StatusCode != http.StatusOK {
						t.Fatalf("loopback discovery status: %d", resp.StatusCode)
					}

					break
				}

				if time.Now().After(deadline) {
					t.Fatalf("loopback discovery failed: %v", err)
				}

				time.Sleep(20 * time.Millisecond)
			}

			anonymous, err := rest.HTTPClientFor(rest.AnonymousClientConfig(loopback))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(anonymous.CloseIdleConnections)

			resp, err := anonymous.Get(apiURL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			want := http.StatusUnauthorized
			if allowAnonymous {
				want = http.StatusForbidden
			}

			if resp.StatusCode != want {
				t.Fatalf("anonymous discovery status: %d, want %d", resp.StatusCode, want)
			}
		})
	}
}

func TestServerClosesListenerOnSetupFailure(t *testing.T) {
	for _, phase := range []string{"secure serving", "authorization"} {
		t.Run(phase, func(t *testing.T) {
			_, kube, _ := fixture(t)

			certPEM, keyPEM, err := certutil.GenerateSelfSignedCertKey("rebound.system.svc", nil, nil)
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
			opts.Authorization.RemoteKubeConfigFile = t.TempDir() + "/missing-kubeconfig"

			if phase == "secure serving" {
				opts.SecureServing.MinTLSVersion = "invalid"
			}

			_, err = opts.NewServer(ServerConfig{
				Client: kube, Namespace: "system", Cert: cert,
				Authenticator: authenticator.RequestFunc(func(*http.Request) (*authenticator.Response, bool, error) {
					return nil, false, nil
				}),
			})
			if err == nil {
				t.Fatal("expected setup failure")
			}

			if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("listener was not closed after setup failure: %v", err)
			}
		})
	}
}

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
	opts.MaxNodesPerGrant = 2

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

	require.Equal(t, 2, s.storage.maxNodesPerGrant)

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

	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
		if !slices.Contains(r.Verbs, verb) {
			t.Fatalf("missing verb %s: %+v", verb, r.Verbs)
		}
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

	data = requestHTTP(http.MethodGet, path+"/http-test", "application/json;as=Table;v=v1;g=meta.k8s.io", nil, http.StatusOK)

	var table metav1.Table
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}

	if table.Kind != "Table" || len(table.Rows) != 1 || len(table.ColumnDefinitions) != 5 {
		t.Fatalf("table: %s", data)
	}

	requestHTTP(http.MethodGet, path+"?fieldSelector=metadata.name%3Dhttp-test", "", nil, http.StatusOK)

	// Patches that change nothing succeed, as `kubectl apply` of an unchanged
	// manifest needs. Any real change is rejected.
	patchHTTP := func(contentType, patch string, code int) {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, base+path+"/http-test", strings.NewReader(patch))
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentType)

		resp, err := client.Do(req)
		require.NoError(t, err)

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, code, resp.StatusCode, "%s %s: %s", contentType, patch, body)
	}
	patchHTTP("application/merge-patch+json", `{"metadata":{"labels":{"team":"blue"}}}`, http.StatusOK)
	patchHTTP("application/strategic-merge-patch+json", `{"metadata":{"annotations":{"note":"test"}}}`, http.StatusOK)
	patchHTTP("application/json-patch+json", `[{"op":"replace","path":"/spec/ttl","value":"5m"}]`, http.StatusUnprocessableEntity)
	patchHTTP("application/merge-patch+json", `{"metadata":{"labels":{"team":"red"}}}`, http.StatusUnprocessableEntity)

	// Clients that prefer protobuf, such as the namespace controller, must fall
	// back to JSON because NodeGrant has no protobuf encoding. Metadata-only
	// lists still use protobuf, which their meta.k8s.io types support.
	var list metav1.TypeMeta
	require.NoError(t, json.Unmarshal(requestHTTP(http.MethodGet, path, "application/vnd.kubernetes.protobuf, application/json", nil, http.StatusOK), &list))
	require.Equal(t, rebound.Kind+"List", list.Kind)
	requestHTTP(http.MethodGet, path, "application/vnd.kubernetes.protobuf;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1,"+
		"application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1,application/json", nil, http.StatusOK)

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

	// kubectl finds the schema for client-side validation by the
	// x-kubernetes-group-version-kind extension. Without it, kubectl falls back
	// to listing CRDs, which most grant users cannot do.
	data = requestHTTP(http.MethodGet, "/openapi/v3/apis/"+rebound.SchemeGroupVersion.String(), "", nil, http.StatusOK)

	var openAPI struct {
		Components struct {
			Schemas map[string]struct {
				GVK []map[string]string `json:"x-kubernetes-group-version-kind"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &openAPI); err != nil {
		t.Fatal(err)
	}

	model := openAPI.Components.Schemas[rebound.NodeGrant{}.OpenAPIModelName()]

	wantGVK := map[string]string{"group": rebound.GroupName, "version": rebound.SchemeGroupVersion.Version, "kind": rebound.Kind}
	if len(model.GVK) != 1 || !maps.Equal(model.GVK[0], wantGVK) {
		t.Fatalf("NodeGrant schema GVK: %+v", model.GVK)
	}

	// kubectl only trusts the schema for validation when the PATCH operation
	// on the resource carries the same extension.
	var openAPIPaths struct {
		Paths map[string]struct {
			Patch struct {
				GVK map[string]string `json:"x-kubernetes-group-version-kind"`
			} `json:"patch"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(data, &openAPIPaths))
	require.Equal(t, wantGVK, openAPIPaths.Paths[apiPath+"/namespaces/{namespace}/nodegrants/{name}"].Patch.GVK)

	requestHTTP(http.MethodGet, path+"?watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan", "", nil, http.StatusBadRequest)
	denied.Store(true)
	requestHTTP(http.MethodGet, path, "", nil, http.StatusForbidden)

	for _, health := range []string{"/readyz", "/livez", "/healthz"} {
		requestHTTP(http.MethodGet, health, "", nil, http.StatusOK)
	}
}
