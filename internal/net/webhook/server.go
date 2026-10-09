// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednet "github.com/Azure/unbounded/internal/net/client/unboundednet"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	defaultServiceName           = "unbounded-net-webhook"
	extensionAuthNamespace       = "kube-system"
	extensionAuthConfigMapName   = "extension-apiserver-authentication"
	extensionAuthClientCAKey     = "requestheader-client-ca-file"
	extensionAuthAllowedNamesKey = "requestheader-allowed-names"
	aggregatedAPIGroupPath       = "/apis/status.net.unbounded-cloud.io"
	aggregatedAPIVersionPath     = "/apis/status.net.unbounded-cloud.io/v1alpha1"
)

// NodeSiteResolver provides site matching without allocating pod CIDRs.
type NodeSiteResolver interface {
	GetSiteForNode(node *corev1.Node) string
}

// Server is a handler registrar for validating and mutating admission
// webhooks plus aggregated API discovery endpoints. It does not own an HTTP
// server or manage TLS certificates -- callers register its handlers on an
// externally-managed mux and serve it with their own TLS configuration.
type Server struct {
	clientset                  kubernetes.Interface
	restConfig                 *rest.Config
	namespace                  string
	serviceName                string
	validator                  *Validator
	aggregatedClientCAs        *x509.CertPool
	aggregatedClientAllowedCNs map[string]struct{}
	mux                        *http.ServeMux
	nodeSiteResolver           NodeSiteResolver
}

// SetNodeSiteResolver sets the site resolver used by the mutating webhook.
func (s *Server) SetNodeSiteResolver(resolver NodeSiteResolver) {
	s.nodeSiteResolver = resolver
}

// NewServer creates a webhook handler registrar. It does not start any HTTP
// server; call RegisterHandlers to wire routes onto the internal mux and then
// serve the mux externally.
func NewServer(clientset kubernetes.Interface, restConfig *rest.Config, namespace string) (*Server, error) {
	siteClient, err := unboundednet.NewSiteClient(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create site client: %w", err)
	}

	poolClient, err := unboundednet.NewGatewayPoolClient(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway pool client: %w", err)
	}

	validator := &Validator{siteClient: siteClient, poolClient: poolClient, clientset: clientset}

	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}

	if namespace == "" {
		namespace = unbounded.SystemNamespace()
	}

	mux := http.NewServeMux()

	return &Server{
		clientset:   clientset,
		restConfig:  restConfig,
		namespace:   namespace,
		serviceName: defaultServiceName,
		validator:   validator,
		mux:         mux,
	}, nil
}

// RegisterHandlers registers the webhook and aggregated discovery handlers on
// the internal mux and starts a background goroutine that periodically
// refreshes the front-proxy client CA bundle. It does not start an HTTP server.
func (s *Server) RegisterHandlers(ctx context.Context) {
	s.refreshAggregatedClientCAs(ctx)

	s.mux.HandleFunc("/validate", s.handleValidate)
	s.mux.HandleFunc("/mutate-nodes", s.handleMutateNodes)
	s.registerAggregatedDiscoveryHandlers()

	go func() {
		const interval = 24 * time.Hour

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshAggregatedClientCAs(ctx)
			}
		}
	}()
}

// Mux returns the HTTP mux so external code can register handlers on the
// webhook TLS server before it starts.
func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

// IsTrustedAggregatedRequest validates that aggregated API requests arrive with
// a verified client certificate signed by the cluster front-proxy CA.
func (s *Server) IsTrustedAggregatedRequest(r *http.Request) bool {
	return s.isTrustedAggregatedRequest(r)
}

// registerAggregatedDiscoveryHandlers registers the aggregated API group and
// version discovery endpoints. These are called by the Kubernetes API server
// during aggregated API discovery and require front-proxy client cert auth.
func (s *Server) registerAggregatedDiscoveryHandlers() {
	s.mux.HandleFunc(aggregatedAPIGroupPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.isTrustedAggregatedRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIGroup","apiVersion":"v1","name":"status.net.unbounded-cloud.io","versions":[{"groupVersion":"status.net.unbounded-cloud.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"status.net.unbounded-cloud.io/v1alpha1","version":"v1alpha1"}}`)) //nolint:errcheck
	})
	s.mux.HandleFunc(aggregatedAPIVersionPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.isTrustedAggregatedRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"status.net.unbounded-cloud.io/v1alpha1","resources":[{"name":"status/push","singularName":"","namespaced":false,"kind":"NodeStatusPush","verbs":["create"]},{"name":"status/nodews","singularName":"","namespaced":false,"kind":"NodeStatusStream","verbs":["get"]},{"name":"status/json","singularName":"","namespaced":false,"kind":"ClusterStatus","verbs":["get"]},{"name":"nodes/details","singularName":"","namespaced":false,"kind":"NodeDetails","verbs":["get","create"]},{"name":"token/node","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]},{"name":"token/viewer","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]}]}`)) //nolint:errcheck
	})
}

// isTrustedAggregatedRequest validates that aggregated API requests arrive with
// a verified client certificate signed by the cluster trust roots.
func (s *Server) isTrustedAggregatedRequest(r *http.Request) bool {
	if r == nil {
		klog.V(2).Info("Rejecting aggregated request: nil request")
		return false
	}

	reqCtx := requestContextForLog(r)

	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		klog.V(2).Infof("Rejecting aggregated request without client certificate: %s; tls=%t handshakeComplete=%t serverName=%q negotiatedProtocol=%q",
			reqCtx,
			r.TLS != nil,
			r.TLS != nil && r.TLS.HandshakeComplete,
			tlsServerName(r.TLS),
			tlsNegotiatedProto(r.TLS),
		)

		return false
	}

	if s.aggregatedClientCAs == nil {
		klog.Warningf("Rejecting aggregated request because client CA pool is not configured: %s", reqCtx)
		return false
	}

	leaf := r.TLS.PeerCertificates[0]

	intermediates := x509.NewCertPool()
	for _, cert := range r.TLS.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         s.aggregatedClientCAs,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		klog.V(2).Infof("Rejecting aggregated request with untrusted client certificate: %s; %s; verifyErr=%v",
			reqCtx, certDescription(leaf), err)

		return false
	}

	if len(s.aggregatedClientAllowedCNs) > 0 {
		if _, ok := s.aggregatedClientAllowedCNs[leaf.Subject.CommonName]; !ok {
			klog.V(2).Infof("Rejecting aggregated request with unexpected client certificate CN: %s; %s; allowedCNs=%v",
				reqCtx, certDescription(leaf), allowedCNsForLog(s.aggregatedClientAllowedCNs))

			return false
		}
	}

	return true
}

// requestContextForLog returns a single-line summary of an HTTP request
// suitable for inclusion in rejection log lines.
func requestContextForLog(r *http.Request) string {
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}

	return fmt.Sprintf("method=%s path=%q host=%q remoteAddr=%s userAgent=%q",
		r.Method, path, r.Host, r.RemoteAddr, r.UserAgent())
}

// certDescription returns a single-line summary of an X.509 certificate.
func certDescription(c *x509.Certificate) string {
	if c == nil {
		return "cert=<nil>"
	}

	return fmt.Sprintf("certSubject=%q certIssuer=%q certSerial=%s certNotBefore=%s certNotAfter=%s certDNSNames=%v",
		c.Subject.String(),
		c.Issuer.String(),
		c.SerialNumber.String(),
		c.NotBefore.UTC().Format(time.RFC3339),
		c.NotAfter.UTC().Format(time.RFC3339),
		c.DNSNames,
	)
}

// tlsServerName returns the SNI server name from a TLS connection state, or
// the empty string when none was provided.
func tlsServerName(state *tls.ConnectionState) string {
	if state == nil {
		return ""
	}

	return state.ServerName
}

// tlsNegotiatedProto returns the negotiated ALPN protocol or empty string.
func tlsNegotiatedProto(state *tls.ConnectionState) string {
	if state == nil {
		return ""
	}

	return state.NegotiatedProtocol
}

// allowedCNsForLog returns a slice of the allowed CNs for inclusion in logs.
func allowedCNsForLog(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for cn := range set {
		out = append(out, cn)
	}

	sort.Strings(out)

	return out
}

// GetClientCAs returns the front-proxy client CA pool so callers can set it
// on the unified TLS server's ClientCAs. The returned pool may be nil if the
// extension-apiserver-authentication ConfigMap has not been loaded yet.
func (s *Server) GetClientCAs() *x509.CertPool {
	return s.aggregatedClientCAs
}

// RefreshAggregatedClientCAs reloads the front-proxy client CA bundle from
// the extension-apiserver-authentication ConfigMap in kube-system.
func (s *Server) RefreshAggregatedClientCAs(ctx context.Context) {
	s.refreshAggregatedClientCAs(ctx)
}

func (s *Server) refreshAggregatedClientCAs(ctx context.Context) {
	cm, err := s.clientset.CoreV1().ConfigMaps(extensionAuthNamespace).Get(ctx, extensionAuthConfigMapName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to read %s/%s for aggregated API authentication: %v", extensionAuthNamespace, extensionAuthConfigMapName, err)

		s.aggregatedClientCAs = nil
		s.aggregatedClientAllowedCNs = nil

		return
	}

	pool := x509.NewCertPool()

	caPEM := []byte(cm.Data[extensionAuthClientCAKey])
	if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
		klog.Warningf("ConfigMap %s/%s does not contain valid %q PEM data", extensionAuthNamespace, extensionAuthConfigMapName, extensionAuthClientCAKey)

		s.aggregatedClientCAs = nil
		s.aggregatedClientAllowedCNs = nil

		return
	}

	s.aggregatedClientCAs = pool

	allowedNames, parseErr := parseRequestHeaderAllowedNames(cm.Data[extensionAuthAllowedNamesKey])
	if parseErr != nil {
		klog.Warningf("Failed to parse %q from %s/%s: %v", extensionAuthAllowedNamesKey, extensionAuthNamespace, extensionAuthConfigMapName, parseErr)

		s.aggregatedClientAllowedCNs = nil

		return
	}

	s.aggregatedClientAllowedCNs = allowedNames
}

func parseRequestHeaderAllowedNames(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}

	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, err
	}

	if len(names) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}

		allowed[name] = struct{}{}
	}

	if len(allowed) == 0 {
		return nil, nil
	}

	return allowed, nil
}

// handleMutateNodes handles mutating admission requests for node objects.
// It labels newly created nodes with their site. Pod CIDRs are assigned by
// reconciliation after creation, never by admission.
func (s *Server) handleMutateNodes(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	defer func() { _ = r.Body.Close() }() //nolint:errcheck

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		http.Error(w, "failed to unmarshal review", http.StatusBadRequest)
		return
	}

	if review.Request == nil {
		http.Error(w, "missing admission request", http.StatusBadRequest)
		return
	}

	response := &admissionv1.AdmissionResponse{
		UID:     review.Request.UID,
		Allowed: true,
	}

	// Only mutate node CREATE requests
	if review.Request.Operation != admissionv1.Create ||
		review.Request.Resource.Resource != "nodes" {
		writeAdmissionResponse(w, review, response)
		return
	}

	var result string

	if s.nodeSiteResolver != nil {
		var node corev1.Node
		if err := json.Unmarshal(review.Request.Object.Raw, &node); err != nil {
			http.Error(w, "failed to unmarshal node", http.StatusBadRequest)
			return
		}

		if siteName := s.nodeSiteResolver.GetSiteForNode(&node); siteName != "" {
			patch, err := buildNodeAdmissionPatch(&node, siteName)
			if err != nil {
				klog.Errorf("Failed to build site label admission patch for node %s: %v", node.Name, err)
				http.Error(w, "failed to build site label patch", http.StatusInternalServerError)

				return
			}

			patchType := admissionv1.PatchTypeJSONPatch
			response.Patch = patch
			response.PatchType = &patchType
			result = fmt.Sprintf("labeled site=%s", siteName)
		} else {
			result = "no-match"
		}
	} else {
		result = "site-resolver-not-set"
	}

	writeAdmissionResponse(w, review, response)

	dur := time.Since(start)
	klog.Infof("Mutating webhook: node=%s result=%s latency=%v",
		review.Request.Name, result, dur)
}

// buildNodeAdmissionPatch sets only site labels, preserving unrelated labels.
func buildNodeAdmissionPatch(node *corev1.Node, siteName string) ([]byte, error) {
	var patches []map[string]interface{}

	if siteName != "" {
		if len(node.Labels) == 0 {
			patches = append(patches, map[string]interface{}{
				"op": "add", "path": "/metadata/labels", "value": map[string]string{},
			})
		}

		// Publish only the canonical site-membership label.
		for _, key := range nodeSiteLabelKeys() {
			patches = append(patches,
				map[string]interface{}{"op": "add", "path": "/metadata/labels/" + escapeJSONPointer(key), "value": siteName},
			)
		}
	}

	return json.Marshal(patches)
}

// nodeSiteLabelKeys are the node site-membership label keys, canonical first.
func nodeSiteLabelKeys() []string {
	return []string{unboundedv1alpha3.MachineSiteLabelKey}
}

// escapeJSONPointer escapes a string for use in a JSON Pointer path segment
// (RFC 6901): "~" becomes "~0" and "/" becomes "~1".
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")

	return s
}

func writeAdmissionResponse(w http.ResponseWriter, review admissionv1.AdmissionReview, response *admissionv1.AdmissionResponse) {
	review.Response = response
	review.Response.UID = review.Request.UID

	data, err := json.Marshal(review)
	if err != nil {
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data) //nolint:errcheck
}
