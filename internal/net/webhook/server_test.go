// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// TestIsTrustedAggregatedRequest tests is trusted aggregated request.
func TestIsTrustedAggregatedRequest(t *testing.T) {
	clientCertPEM, clientKeyPEM, caPEM, err := generateClientAuthCertificate("front-proxy-client")
	if err != nil {
		t.Fatalf("generateClientAuthCertificate failed: %v", err)
	}

	serverCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("parse keypair failed: %v", err)
	}

	leaf, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf failed: %v", err)
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(caPEM); !ok {
		t.Fatal("failed to append CA cert to pool")
	}

	s := &Server{aggregatedClientCAs: pool}

	trustedReq := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}
	if !s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected trusted aggregated request")
	}

	s.aggregatedClientAllowedCNs = map[string]struct{}{leaf.Subject.CommonName: {}}
	if !s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected trusted aggregated request when CN is allowed")
	}

	s.aggregatedClientAllowedCNs = map[string]struct{}{"not-the-cert-cn": {}}
	if s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected request rejection when client certificate CN is not allowed")
	}

	untrusted := &Server{}
	if untrusted.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected request rejection when no client CA pool configured")
	}

	if s.isTrustedAggregatedRequest(&http.Request{}) {
		t.Fatal("expected request without TLS peer certs to be rejected")
	}
}

func TestAggregatedDiscoveryAdvertisesNodeDetails(t *testing.T) {
	clientCertPEM, _, caPEM, err := generateClientAuthCertificate("front-proxy-client")
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode(clientCertPEM)

	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append front-proxy CA")
	}

	server := &Server{aggregatedClientCAs: pool, mux: http.NewServeMux()}
	server.registerAggregatedDiscoveryHandlers()

	request := httptest.NewRequest(http.MethodGet, aggregatedAPIVersionPath, nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	var discovery metav1.APIResourceList
	if err := json.NewDecoder(response.Body).Decode(&discovery); err != nil {
		t.Fatal(err)
	}

	for _, resource := range discovery.APIResources {
		if resource.Name == "nodes/details" {
			if resource.Kind != "NodeDetails" || !slices.Equal(resource.Verbs, metav1.Verbs{"get", "create"}) {
				t.Fatalf("unexpected nodes/details discovery: %+v", resource)
			}

			return
		}
	}

	t.Fatal("nodes/details missing from aggregated API discovery")
}

func generateClientAuthCertificate(commonName string) ([]byte, []byte, []byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	return clientCertPEM, clientKeyPEM, caPEM, nil
}

// TestParseRequestHeaderAllowedNames tests parse request header allowed names.
func TestParseRequestHeaderAllowedNames(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		allowed, err := parseRequestHeaderAllowedNames("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if allowed != nil {
			t.Fatalf("expected nil map for empty value")
		}
	})

	t.Run("valid", func(t *testing.T) {
		allowed, err := parseRequestHeaderAllowedNames(`["front-proxy-client","aggregator"]`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(allowed) != 2 {
			t.Fatalf("expected two allowed names, got %d", len(allowed))
		}

		if _, ok := allowed["front-proxy-client"]; !ok {
			t.Fatal("expected front-proxy-client to be allowed")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if _, err := parseRequestHeaderAllowedNames(`not-json`); err == nil {
			t.Fatal("expected parse error for invalid JSON")
		}
	})
}

// TestRefreshAggregatedClientCAsFromConfigMap tests refresh aggregated client cas from config map.
func TestRefreshAggregatedClientCAsFromConfigMap(t *testing.T) {
	caPEM, _, _, err := generateClientAuthCertificate("test-ca-subject")
	if err != nil {
		t.Fatalf("generateClientAuthCertificate returned error: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: extensionAuthConfigMapName, Namespace: extensionAuthNamespace},
		Data: map[string]string{
			extensionAuthClientCAKey:     string(caPEM),
			extensionAuthAllowedNamesKey: `["front-proxy-client"]`,
		},
	}

	clientset := fake.NewClientset(cm)
	s := &Server{clientset: clientset}
	s.refreshAggregatedClientCAs(t.Context())

	if s.aggregatedClientCAs == nil {
		t.Fatal("expected aggregated client CA pool to be loaded")
	}

	if len(s.aggregatedClientAllowedCNs) != 1 {
		t.Fatalf("expected one allowed client CN, got %d", len(s.aggregatedClientAllowedCNs))
	}

	if _, ok := s.aggregatedClientAllowedCNs["front-proxy-client"]; !ok {
		t.Fatal("expected front-proxy-client to be allowed")
	}
}

// TestRegisterHandlers_ContextCancel tests that the CA refresh goroutine
// started by RegisterHandlers exits when the context is canceled.
func TestRegisterHandlers_ContextCancel(t *testing.T) {
	clientset := fake.NewClientset()
	s := &Server{
		clientset:   clientset,
		namespace:   "kube-system",
		serviceName: defaultServiceName,
		mux:         http.NewServeMux(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	s.RegisterHandlers(ctx)
	cancel()
	// No assertion needed beyond ensuring no panic/deadlock.
}

// TestGetClientCAs tests that GetClientCAs returns the front-proxy CA pool.
func TestGetClientCAs(t *testing.T) {
	s := &Server{}
	if s.GetClientCAs() != nil {
		t.Fatal("expected nil client CAs before refresh")
	}

	pool := x509.NewCertPool()

	s.aggregatedClientCAs = pool
	if got := s.GetClientCAs(); got != pool {
		t.Fatal("expected GetClientCAs to return the set pool")
	}
}

// TestBuildNodeAdmissionPatchDualWritesSiteLabels verifies the mutating webhook
// stamps both the canonical (unbounded-cloud.io/site) and deprecated
// (net.unbounded-cloud.io/site) site labels during the deprecation window.
func TestBuildNodeAdmissionPatchDualWritesSiteLabels(t *testing.T) {
	patch, err := buildNodeAdmissionPatch(&corev1.Node{}, "site-a")
	if err != nil {
		t.Fatalf("build patch: %v", err)
	}

	var ops []map[string]interface{}
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}

	labelValues := map[string]interface{}{}

	for _, op := range ops {
		path, _ := op["path"].(string)
		if !strings.HasPrefix(path, "/metadata/labels") {
			t.Fatalf("unexpected non-label mutation: %#v", op)
		}

		if strings.HasPrefix(path, "/metadata/labels/") {
			labelValues[path] = op["value"]
		}
	}

	canonical := "/metadata/labels/" + escapeJSONPointer(unboundedv1alpha3.MachineSiteLabelKey)
	deprecated := "/metadata/labels/" + escapeJSONPointer(unboundednetv1alpha1.SiteLabelKey)

	if labelValues[canonical] != "site-a" {
		t.Fatalf("canonical site label not set: %#v", labelValues)
	}

	if labelValues[deprecated] != "site-a" {
		t.Fatalf("deprecated site label not set: %#v", labelValues)
	}
}

type fakeNodeSiteResolver struct {
	siteName string
	calls    int
}

func (f *fakeNodeSiteResolver) GetSiteForNode(_ *corev1.Node) string {
	f.calls++
	return f.siteName
}

func TestMutateNodesOnlyLabelsSite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     map[string]string
		cidrs      []string
		siteName   string
		operation  admissionv1.Operation
		noResolver bool
		dryRun     bool
	}{
		{name: "absent-labels", siteName: "site-a", operation: admissionv1.Create},
		{name: "empty-labels", labels: map[string]string{}, siteName: "site-a", operation: admissionv1.Create},
		{name: "existing-labels", labels: map[string]string{"other": "keep", unboundedv1alpha3.MachineSiteLabelKey: "old"}, siteName: "site-a", operation: admissionv1.Create},
		{name: "existing-cidrs", cidrs: []string{"10.0.0.0/24", "fd00::/64"}, siteName: "site-a", operation: admissionv1.Create},
		{name: "dry-run", siteName: "site-a", operation: admissionv1.Create, dryRun: true},
		{name: "no-match", operation: admissionv1.Create},
		{name: "no-resolver", operation: admissionv1.Create, noResolver: true},
		{name: "update", siteName: "site-a", operation: admissionv1.Update},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: tc.labels}}

			node.Spec.PodCIDRs = tc.cidrs
			if len(tc.cidrs) > 0 {
				node.Spec.PodCIDR = tc.cidrs[0]
			}

			raw, err := json.Marshal(node)
			if err != nil {
				t.Fatal(err)
			}

			review := admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{
				UID: "request-a", Name: node.Name, Operation: tc.operation,
				Resource: metav1.GroupVersionResource{Version: "v1", Resource: "nodes"},
				Object:   runtime.RawExtension{Raw: raw}, DryRun: &tc.dryRun,
			}}

			body, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}

			resolver := &fakeNodeSiteResolver{siteName: tc.siteName}

			server := &Server{}
			if !tc.noResolver {
				server.SetNodeSiteResolver(resolver)
			}

			rec := httptest.NewRecorder()
			server.handleMutateNodes(rec, httptest.NewRequest(http.MethodPost, "/mutate-nodes", strings.NewReader(string(body))))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}

			var result admissionv1.AdmissionReview
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}

			if result.Response == nil || !result.Response.Allowed || result.Response.UID != review.Request.UID {
				t.Fatalf("invalid admission response: %#v", result.Response)
			}

			wantPatch := tc.operation == admissionv1.Create && tc.siteName != "" && !tc.noResolver
			if !wantPatch {
				if len(result.Response.Patch) != 0 || result.Response.PatchType != nil {
					t.Fatalf("unexpected mutation: %s", result.Response.Patch)
				}

				if tc.operation != admissionv1.Create && resolver.calls != 0 {
					t.Fatal("resolved a site for a non-CREATE request")
				}

				return
			}

			if result.Response.PatchType == nil || *result.Response.PatchType != admissionv1.PatchTypeJSONPatch {
				t.Fatal("missing JSON patch type")
			}

			patch, err := jsonpatch.DecodePatch(result.Response.Patch)
			if err != nil {
				t.Fatal(err)
			}

			mutated, err := patch.Apply(raw)
			if err != nil {
				t.Fatalf("apply label patch: %v", err)
			}

			var got corev1.Node
			if err := json.Unmarshal(mutated, &got); err != nil {
				t.Fatal(err)
			}

			for _, key := range nodeSiteLabelKeys() {
				if got.Labels[key] != tc.siteName {
					t.Fatalf("label %s = %q, want %q", key, got.Labels[key], tc.siteName)
				}
			}

			if value, ok := tc.labels["other"]; ok && got.Labels["other"] != value {
				t.Fatal("unrelated label changed")
			}

			if got.Spec.PodCIDR != node.Spec.PodCIDR || !slices.Equal(got.Spec.PodCIDRs, node.Spec.PodCIDRs) {
				t.Fatalf("admission changed pod CIDRs: %+v", got.Spec)
			}
		})
	}
}

func TestMutateNodesInvalidRequest(t *testing.T) {
	for _, body := range []string{"{", "{}", `{"request":{"operation":"CREATE","resource":{"resource":"nodes"},"object":[]}}`} {
		t.Run(body, func(t *testing.T) {
			server := &Server{nodeSiteResolver: &fakeNodeSiteResolver{siteName: "site-a"}}
			rec := httptest.NewRecorder()
			server.handleMutateNodes(rec, httptest.NewRequest(http.MethodPost, "/mutate-nodes", strings.NewReader(body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}
