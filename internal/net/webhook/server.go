// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	unboundednet "github.com/Azure/unbounded/internal/net/client/unboundednet"
)

const (
	defaultServiceName           = "unbounded-net-webhook"
	extensionAuthNamespace       = "kube-system"
	extensionAuthConfigMapName   = "extension-apiserver-authentication"
	extensionAuthClientCAKey     = "requestheader-client-ca-file"
	extensionAuthAllowedNamesKey = "requestheader-allowed-names"
	aggregatedAPIGroupPath       = "/apis/status.net.unbounded-kube.io"
	aggregatedAPIVersionPath     = "/apis/status.net.unbounded-kube.io/v1alpha1"
)

// CIDRAllocator provides pod CIDR allocation for the mutating webhook.
type CIDRAllocator interface {
	// TryAllocateForNode attempts to allocate pod CIDRs for a node.
	// Returns (podCIDR, podCIDRs, siteName, true) on success or
	// ("", nil, "", false) if allocation is not possible.
	TryAllocateForNode(nodeName string, internalIPs []string) (string, []string, string, bool)
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
	cidrAllocator              CIDRAllocator
}

// SetCIDRAllocator sets the CIDR allocator used by the mutating webhook.
func (s *Server) SetCIDRAllocator(a CIDRAllocator) {
	s.cidrAllocator = a
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
		namespace = "kube-system"
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
		_, _ = w.Write([]byte(`{"kind":"APIGroup","apiVersion":"v1","name":"status.net.unbounded-kube.io","versions":[{"groupVersion":"status.net.unbounded-kube.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"status.net.unbounded-kube.io/v1alpha1","version":"v1alpha1"}}`)) //nolint:errcheck
	})
	s.mux.HandleFunc(aggregatedAPIVersionPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.isTrustedAggregatedRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"status.net.unbounded-kube.io/v1alpha1","resources":[{"name":"status/push","singularName":"","namespaced":false,"kind":"NodeStatusPush","verbs":["create"]},{"name":"status/nodews","singularName":"","namespaced":false,"kind":"NodeStatusStream","verbs":["get"]},{"name":"status/json","singularName":"","namespaced":false,"kind":"ClusterStatus","verbs":["get"]},{"name":"token/node","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]},{"name":"token/viewer","singularName":"","namespaced":false,"kind":"TokenRequest","verbs":["create"]}]}`)) //nolint:errcheck
	})
}

// isTrustedAggregatedRequest validates that aggregated API requests arrive with
// a verified client certificate signed by the cluster trust roots.
func (s *Server) isTrustedAggregatedRequest(r *http.Request) bool {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		klog.V(2).Info("Rejecting aggregated request without client certificate")
		return false
	}

	if s.aggregatedClientCAs == nil {
		klog.Warning("Rejecting aggregated request because client CA pool is not configured")
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
		klog.V(2).Infof("Rejecting aggregated request with untrusted client certificate: %v", err)
		return false
	}

	if len(s.aggregatedClientAllowedCNs) > 0 {
		if _, ok := s.aggregatedClientAllowedCNs[leaf.Subject.CommonName]; !ok {
			klog.V(2).Infof("Rejecting aggregated request with unexpected client certificate CN %q", leaf.Subject.CommonName)
			return false
		}
	}

	return true
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
// It attempts to allocate pod CIDRs for newly created nodes that do not
// already have CIDRs assigned.
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

	result := "pass-through"

	if s.cidrAllocator != nil {
		var node corev1.Node
		if err := json.Unmarshal(review.Request.Object.Raw, &node); err == nil {
			if len(node.Spec.PodCIDRs) == 0 {
				var ips []string

				for _, addr := range node.Status.Addresses {
					if addr.Type == corev1.NodeInternalIP {
						ips = append(ips, addr.Address)
					}
				}

				if podCIDR, podCIDRs, siteName, ok := s.cidrAllocator.TryAllocateForNode(node.Name, ips); ok {
					patch := buildNodeAdmissionPatch(podCIDR, podCIDRs, siteName)
					patchType := admissionv1.PatchTypeJSONPatch
					response.Patch = patch
					response.PatchType = &patchType
					result = fmt.Sprintf("allocated podCIDR=%s site=%s", podCIDR, siteName)
				} else {
					result = "no-match"
				}
			} else {
				result = "already-has-cidrs"
			}
		}
	} else {
		result = "allocator-not-set"
	}

	writeAdmissionResponse(w, review, response)

	dur := time.Since(start)
	klog.Infof("Mutating webhook: node=%s result=%s latency=%v",
		review.Request.Name, result, dur)
}

// buildNodeAdmissionPatch creates a JSONPatch that sets podCIDR, podCIDRs,
// and the site label on a node during admission.
func buildNodeAdmissionPatch(podCIDR string, podCIDRs []string, siteName string) []byte {
	patches := []map[string]interface{}{
		{"op": "add", "path": "/spec/podCIDR", "value": podCIDR},
		{"op": "add", "path": "/spec/podCIDRs", "value": podCIDRs},
	}
	if siteName != "" {
		patches = append(patches,
			map[string]interface{}{"op": "add", "path": "/metadata/labels/net.unbounded-kube.io~1site", "value": siteName},
		)
	}

	data, _ := json.Marshal(patches) //nolint:errcheck

	return data
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
