// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	apiregclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"

	apiGroup        = "playpen.unbounded-cloud.io"
	apiVersion      = "v1alpha1"
	apiGroupVersion = apiGroup + "/" + apiVersion
	apiServiceName  = apiVersion + "." + apiGroup

	aggregatedAPIGroupPath   = "/apis/" + apiGroup
	aggregatedAPIVersionPath = aggregatedAPIGroupPath + "/" + apiVersion
	allocsPath               = aggregatedAPIVersionPath + "/allocs"
	deallocsPath             = aggregatedAPIVersionPath + "/deallocs"

	extensionAuthNamespace       = "kube-system"
	extensionAuthConfigMapName   = "extension-apiserver-authentication"
	extensionAuthClientCAKey     = "requestheader-client-ca-file"
	extensionAuthAllowedNamesKey = "requestheader-allowed-names"
	remoteUserHeader             = "X-Remote-User"
	remoteGroupHeader            = "X-Remote-Group"
)

type Operator struct {
	Client       client.Client
	KubeClient   kubernetes.Interface
	APIRegClient apiregclient.Interface
	Config       Config
	Scheme       *runtime.Scheme

	aggregatedMu               sync.RWMutex
	aggregatedClientCAs        *x509.CertPool
	aggregatedClientAllowedCNs map[string]struct{}
}

type AllocRequest struct {
	WireGuardPublicKey string `json:"wireGuardPublicKey"`
	Architecture       string `json:"architecture,omitempty"`
}

type AllocResponse struct {
	Pod       PodResponse       `json:"pod"`
	Endpoint  EndpointResponse  `json:"endpoint"`
	WireGuard WireGuardResponse `json:"wireGuard"`
	VXLAN     VXLANResponse     `json:"vxlan"`
	Network   NetworkResponse   `json:"network"`
	Redfish   map[string]string `json:"redfish"`
}

type PodResponse struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	NodeName     string `json:"nodeName"`
	NodePublicIP string `json:"nodePublicIP"`
	Architecture string `json:"architecture"`
}

type EndpointResponse struct {
	Host                  string `json:"host"`
	WireGuardUDPPort      int32  `json:"wireGuardUDPPort"`
	ExternalTrafficPolicy string `json:"externalTrafficPolicy"`
}

type WireGuardResponse struct {
	Interface       string `json:"interface"`
	ServerPublicKey string `json:"serverPublicKey"`
	ServerAddress   string `json:"serverAddress"`
	ClientAddress   string `json:"clientAddress"`
	ListenPort      int    `json:"listenPort"`
}

type VXLANResponse struct {
	Interface     string `json:"interface"`
	VNI           int    `json:"vni"`
	UDPPort       int    `json:"udpPort"`
	ServerAddress string `json:"serverAddress"`
	ClientAddress string `json:"clientAddress"`
}

type NetworkResponse struct {
	GuestMAC    string   `json:"guestMAC"`
	GuestIPv4   string   `json:"guestIPv4"`
	SubnetMask  string   `json:"subnetMask"`
	GatewayIPv4 string   `json:"gatewayIPv4"`
	DNS         []string `json:"dns"`
}

func (o *Operator) Run(ctx context.Context) error {
	cert, err := o.EnsureTLSSecret(ctx)
	if err != nil {
		return err
	}

	certPEM, err := o.servingCertPEM(ctx)
	if err != nil {
		return err
	}

	if err := o.injectAPIServiceCABundle(ctx, certPEM); err != nil {
		return err
	}

	o.refreshAggregatedClientCAs(ctx)

	server := &http.Server{
		Addr:              o.Config.ListenAddr,
		Handler:           o.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequestClientCert,
			ClientCAs:    o.aggregatedClientCAs,
		},
	}

	errCh := make(chan error, 1)

	go func() {
		errCh <- server.ListenAndServeTLS("", "")
	}()

	ticker := time.NewTicker(o.Config.ReconcileInterval)
	defer ticker.Stop()

	o.ReconcileRunners(ctx) //nolint:errcheck // Periodic reconciliation below will retry transient startup failures.

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			server.Shutdown(shutdownCtx) //nolint:errcheck // Process is exiting after context cancellation.

			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
			o.refreshAggregatedClientCAs(ctx)
			o.ReconcileRunners(ctx) //nolint:errcheck // Next tick retries transient reconciliation failures.
		}
	}
}

func (o *Operator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(aggregatedAPIGroupPath, o.handleAPIGroupDiscovery)
	mux.HandleFunc(aggregatedAPIVersionPath, o.handleAPIVersionDiscovery)
	mux.HandleFunc(allocsPath, o.handleAllocs)
	mux.HandleFunc(deallocsPath, o.handleDeallocs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	return mux
}

func (o *Operator) handleAPIGroupDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != aggregatedAPIGroupPath {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIGroup",
		"apiVersion": "v1",
		"name":       apiGroup,
		"versions": []map[string]string{
			{"groupVersion": apiGroupVersion, "version": apiVersion},
		},
		"preferredVersion": map[string]string{"groupVersion": apiGroupVersion, "version": apiVersion},
	})
}

func (o *Operator) handleAPIVersionDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != aggregatedAPIVersionPath {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": apiGroupVersion,
		"resources": []map[string]any{
			{"name": "allocs", "singularName": "alloc", "namespaced": false, "kind": "Alloc", "verbs": []string{"create"}},
			{"name": "deallocs", "singularName": "dealloc", "namespaced": false, "kind": "Dealloc", "verbs": []string{"create"}},
		},
	})
}

func (o *Operator) handleAllocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.authorizeAggregatedRequest(w, r) {
		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)

		return
	}

	var req AllocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)

		return
	}

	req.WireGuardPublicKey = strings.TrimSpace(req.WireGuardPublicKey)
	if _, err := wgtypes.ParseKey(req.WireGuardPublicKey); err != nil {
		http.Error(w, "wireGuardPublicKey must be a valid WireGuard public key", http.StatusBadRequest)

		return
	}

	arch, err := normalizeArchitecture(req.Architecture)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	req.Architecture = arch

	resp, status, err := o.Alloc(r.Context(), idempotencyKey, req)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (o *Operator) handleDeallocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.authorizeAggregatedRequest(w, r) {
		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)

		return
	}

	status, err := o.Dealloc(r.Context(), idempotencyKey)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (o *Operator) Alloc(ctx context.Context, idempotencyKey string, req AllocRequest) (AllocResponse, int, error) {
	arch, err := normalizeArchitecture(req.Architecture)
	if err != nil {
		return AllocResponse{}, http.StatusBadRequest, err
	}

	req.Architecture = arch

	keyHash := hashString(idempotencyKey)
	reqHash := hashString(req.WireGuardPublicKey + "\n" + req.Architecture)

	pods, err := o.listRunnerPods(ctx)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != keyHash {
			continue
		}

		if pod.Annotations[AnnotationRequestHash] != reqHash {
			return AllocResponse{}, http.StatusConflict, fmt.Errorf("idempotency key was already used with a different request")
		}

		resp, err := o.responseForPod(ctx, pod)
		if err != nil {
			return AllocResponse{}, http.StatusServiceUnavailable, err
		}

		return resp, http.StatusOK, nil
	}

	var unavailable []string

	matchingArchitecture := false

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
			continue
		}

		if podArchitecture(pod) != req.Architecture {
			continue
		}

		matchingArchitecture = true

		if reason := runnerPodUnavailableReason(pod); reason != "" {
			unavailable = append(unavailable, reason)

			continue
		}

		serverPublicKey, err := serverWireGuardPublicKeyForPod(pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod.Spec.NodeName)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		allocatedPod, claimed, err := o.patchClaim(ctx, pod, keyHash, reqHash, req.WireGuardPublicKey)
		if err != nil {
			if errors.Is(err, errRunnerAlreadyAllocated) {
				continue
			}

			if errors.Is(err, errIdempotencyRequestConflict) {
				return AllocResponse{}, http.StatusConflict, err
			}

			return AllocResponse{}, http.StatusInternalServerError, err
		}

		if !claimed {
			resp, err := o.responseForPod(ctx, allocatedPod)
			if err != nil {
				return AllocResponse{}, http.StatusServiceUnavailable, err
			}

			return resp, http.StatusOK, nil
		}

		service, err := o.ensureNodePortService(ctx, allocatedPod)
		if err != nil {
			return AllocResponse{}, http.StatusInternalServerError, err
		}

		resp, err := o.buildResponse(allocatedPod, gatewayIP, nodePublicIP, service, serverPublicKey)
		if err != nil {
			return AllocResponse{}, http.StatusInternalServerError, err
		}

		return resp, http.StatusOK, nil
	}

	if len(unavailable) > 0 {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available with an ExternalIP gateway: %s", strings.Join(unavailable, "; "))
	}

	if !matchingArchitecture {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle %s playpen runner pods are available", req.Architecture)
	}

	return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available")
}

func (o *Operator) Dealloc(ctx context.Context, idempotencyKey string) (int, error) {
	keyHash := hashString(idempotencyKey)

	pods, err := o.listRunnerPods(ctx)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != keyHash {
			continue
		}

		if err := o.deleteRunnerPod(ctx, pod); err != nil {
			return http.StatusInternalServerError, err
		}

		return http.StatusNoContent, nil
	}

	return http.StatusNoContent, nil
}

func (o *Operator) ReconcileRunners(ctx context.Context) error {
	pods, err := o.listRunnerPods(ctx)
	if err != nil {
		return err
	}

	activePods := map[string]struct{}{}
	now := time.Now()

	for i := range pods.Items {
		pod := &pods.Items[i]
		if o.claimExpired(pod, now) {
			if err := o.deleteRunnerPod(ctx, pod); err != nil {
				return err
			}

			continue
		}

		activePods[pod.Name] = struct{}{}
	}

	return o.deleteStaleServices(ctx, activePods)
}

func (o *Operator) EnsureTLSSecret(ctx context.Context) (tls.Certificate, error) {
	secret := &corev1.Secret{}

	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}
	if err := o.Client.Get(ctx, key, secret); err == nil {
		cert, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return tls.Certificate{}, err
		}

		return cert, nil
	} else if !apierrors.IsNotFound(err) {
		return tls.Certificate{}, err
	}

	serviceName := strings.TrimSpace(o.Config.ServiceName)
	if serviceName == "" {
		serviceName = "playpen-operator"
	}

	certPEM, keyPEM, err := selfSignedCert(serviceName, []string{
		serviceName,
		serviceName + "." + o.Config.Namespace,
		serviceName + "." + o.Config.Namespace + ".svc",
		serviceName + "." + o.Config.Namespace + ".svc.cluster.local",
		"localhost",
	})
	if err != nil {
		return tls.Certificate{}, err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: o.Config.TLSSecretName, Namespace: o.Config.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	if err := o.Client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}

func (o *Operator) servingCertPEM(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}

	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}
	if err := o.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}

	certPEM := secret.Data[corev1.TLSCertKey]
	if len(certPEM) == 0 {
		return nil, fmt.Errorf("secret %s/%s is missing %s", o.Config.Namespace, o.Config.TLSSecretName, corev1.TLSCertKey)
	}

	return certPEM, nil
}

func (o *Operator) injectAPIServiceCABundle(ctx context.Context, caBundle []byte) error {
	if o.APIRegClient == nil || len(caBundle) == 0 {
		return nil
	}

	apiService, err := o.APIRegClient.ApiregistrationV1().APIServices().Get(ctx, apiServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.Warningf("APIService %s not found; skipping caBundle injection", apiServiceName)

		return nil
	}

	if err != nil {
		return fmt.Errorf("get APIService %s: %w", apiServiceName, err)
	}

	if bytes.Equal(apiService.Spec.CABundle, caBundle) {
		return nil
	}

	apiService.Spec.CABundle = caBundle
	if _, err := o.APIRegClient.ApiregistrationV1().APIServices().Update(ctx, apiService, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update APIService %s caBundle: %w", apiServiceName, err)
	}

	return nil
}

func (o *Operator) refreshAggregatedClientCAs(ctx context.Context) {
	if o.KubeClient == nil {
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	cm, err := o.KubeClient.CoreV1().ConfigMaps(extensionAuthNamespace).Get(ctx, extensionAuthConfigMapName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to read %s/%s: %v", extensionAuthNamespace, extensionAuthConfigMapName, err)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	pool := x509.NewCertPool()

	caPEM := []byte(cm.Data[extensionAuthClientCAKey])
	if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
		klog.Warningf("ConfigMap %s/%s does not contain valid %q PEM data", extensionAuthNamespace, extensionAuthConfigMapName, extensionAuthClientCAKey)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	allowedNames, err := parseRequestHeaderAllowedNames(cm.Data[extensionAuthAllowedNamesKey])
	if err != nil {
		klog.Warningf("Failed to parse %q from %s/%s: %v", extensionAuthAllowedNamesKey, extensionAuthNamespace, extensionAuthConfigMapName, err)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	o.aggregatedMu.Lock()
	defer o.aggregatedMu.Unlock()

	o.aggregatedClientCAs = pool
	o.aggregatedClientAllowedCNs = allowedNames
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

func (o *Operator) authorizeAggregatedRequest(w http.ResponseWriter, r *http.Request) bool {
	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	user := strings.TrimSpace(r.Header.Get(remoteUserHeader))
	if user == "" {
		http.Error(w, "missing remote user", http.StatusForbidden)

		return false
	}

	if o.KubeClient == nil {
		http.Error(w, "authorization client is not configured", http.StatusInternalServerError)

		return false
	}

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: remoteGroups(r),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:     "create",
				Group:    apiGroup,
				Resource: "allocs",
			},
		},
	}

	result, err := o.KubeClient.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), sar, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, "authorization check failed", http.StatusInternalServerError)

		return false
	}

	if !result.Status.Allowed || result.Status.Denied {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	return true
}

func remoteGroups(r *http.Request) []string {
	var groups []string

	for _, value := range r.Header.Values(remoteGroupHeader) {
		for _, group := range strings.Split(value, ",") {
			group = strings.TrimSpace(group)
			if group != "" {
				groups = append(groups, group)
			}
		}
	}

	return groups
}

func (o *Operator) isTrustedAggregatedRequest(r *http.Request) bool {
	o.aggregatedMu.RLock()
	clientCAs := o.aggregatedClientCAs
	allowedCNs := o.aggregatedClientAllowedCNs
	o.aggregatedMu.RUnlock()

	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || clientCAs == nil {
		return false
	}

	leaf := r.TLS.PeerCertificates[0]

	intermediates := x509.NewCertPool()
	for _, cert := range r.TLS.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         clientCAs,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return false
	}

	if len(allowedCNs) == 0 {
		return true
	}

	_, ok := allowedCNs[leaf.Subject.CommonName]

	return ok
}

func (o *Operator) listRunnerPods(ctx context.Context) (*corev1.PodList, error) {
	selector, err := labels.Parse(o.Config.RunnerLabelSelector)
	if err != nil {
		return nil, err
	}

	list := &corev1.PodList{}
	if err := o.Client.List(ctx, list, client.InNamespace(o.Config.RunnerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}

	return list, nil
}

func runnerPodUnavailableReason(pod *corev1.Pod) string {
	if !pod.DeletionTimestamp.IsZero() {
		return fmt.Sprintf("runner pod %s is terminating", pod.Name)
	}

	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Sprintf("runner pod %s is %s", pod.Name, pod.Status.Phase)
	}

	if len(pod.Status.ContainerStatuses) == 0 {
		return fmt.Sprintf("runner pod %s has no container status", pod.Name)
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running == nil {
			return fmt.Sprintf("runner pod %s container %s is not running", pod.Name, status.Name)
		}

		if !status.Ready {
			return fmt.Sprintf("runner pod %s container %s is not ready", pod.Name, status.Name)
		}
	}

	return ""
}

var (
	errRunnerAlreadyAllocated     = errors.New("runner pod was already allocated")
	errIdempotencyRequestConflict = errors.New("idempotency key was already used with a different request")
)

func (o *Operator) patchClaim(ctx context.Context, pod *corev1.Pod, keyHash, reqHash, clientPublicKey string) (*corev1.Pod, bool, error) {
	var updated *corev1.Pod

	claimed := false

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1.Pod{}
		if err := o.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, current); err != nil {
			return err
		}

		if current.Annotations[AnnotationIdempotencyKeyHash] == keyHash {
			if current.Annotations[AnnotationRequestHash] != reqHash {
				return errIdempotencyRequestConflict
			}

			updated = current
			claimed = false

			return nil
		}

		if current.Annotations[AnnotationIdempotencyKeyHash] != "" {
			return errRunnerAlreadyAllocated
		}

		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}

		if current.Labels == nil {
			current.Labels = map[string]string{}
		}

		allocationID := allocationIDForPod(pod)
		current.Annotations[AnnotationIdempotencyKeyHash] = keyHash
		current.Annotations[AnnotationRequestHash] = reqHash
		current.Annotations[AnnotationClientWireGuardPublicKey] = clientPublicKey
		current.Annotations[AnnotationClaimedAt] = time.Now().UTC().Format(time.RFC3339)
		current.Labels[LabelAllocated] = allocationID

		if err := o.Client.Update(ctx, current); err != nil {
			return err
		}

		updated = current
		claimed = true

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return updated, claimed, nil
}

func (o *Operator) responseForPod(ctx context.Context, pod *corev1.Pod) (AllocResponse, error) {
	serverPublicKey, err := serverWireGuardPublicKeyForPod(pod)
	if err != nil {
		return AllocResponse{}, err
	}

	gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod.Spec.NodeName)
	if err != nil {
		return AllocResponse{}, err
	}

	service, err := o.ensureNodePortService(ctx, pod)
	if err != nil {
		return AllocResponse{}, err
	}

	return o.buildResponse(pod, gatewayIP, nodePublicIP, service, serverPublicKey)
}

func (o *Operator) ensureNodePortService(ctx context.Context, pod *corev1.Pod) (*corev1.Service, error) {
	name := serviceNameForPod(pod)
	service := &corev1.Service{}

	key := types.NamespacedName{Namespace: pod.Namespace, Name: name}
	if err := o.Client.Get(ctx, key, service); err == nil {
		return service, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pod.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":         "playpen-runner",
				"app.kubernetes.io/component":    "runner-nodeport",
				"playpen.unbounded-cloud.io/pod": pod.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeNodePort,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			Selector: map[string]string{
				LabelAllocated: allocationIDForPod(pod),
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "wireguard",
					Protocol:   corev1.ProtocolUDP,
					Port:       int32(o.Config.Runner.WireGuard.ListenPort), //nolint:gosec // User-configured Kubernetes port.
					TargetPort: intstr.FromInt(o.Config.Runner.WireGuard.ListenPort),
				},
			},
		},
	}
	if err := o.Client.Create(ctx, service); apierrors.IsAlreadyExists(err) {
		if err := o.Client.Get(ctx, key, service); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return service, nil
}

func (o *Operator) buildResponse(pod *corev1.Pod, gatewayIP, nodePublicIP string, service *corev1.Service, serverPublicKey string) (AllocResponse, error) {
	runnerCfg := o.Config.Runner

	nodePort := int32(0)
	if len(service.Spec.Ports) > 0 {
		nodePort = service.Spec.Ports[0].NodePort
	}

	serverWG, err := runnerAddressIP(runnerCfg.WireGuard.ServerAddress)
	if err != nil {
		return AllocResponse{}, err
	}

	clientWG, err := runnerAddressIP(runnerCfg.WireGuard.ClientAddress)
	if err != nil {
		return AllocResponse{}, err
	}

	redfishURL := runnerCfg.PublicRedfishURL
	if redfishURL == "" {
		redfishURL = defaultRunnerPublicRedfishURL(runnerCfg)
	}

	return AllocResponse{
		Pod: PodResponse{
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			NodeName:     pod.Spec.NodeName,
			NodePublicIP: nodePublicIP,
			Architecture: podArchitecture(pod),
		},
		Endpoint: EndpointResponse{
			Host:                  gatewayIP,
			WireGuardUDPPort:      nodePort,
			ExternalTrafficPolicy: string(service.Spec.ExternalTrafficPolicy),
		},
		WireGuard: WireGuardResponse{
			Interface:       runnerCfg.WireGuard.Interface,
			ServerPublicKey: serverPublicKey,
			ServerAddress:   runnerCfg.WireGuard.ServerAddress,
			ClientAddress:   runnerCfg.WireGuard.ClientAddress,
			ListenPort:      runnerCfg.WireGuard.ListenPort,
		},
		VXLAN: VXLANResponse{
			Interface:     runnerCfg.VXLAN.Interface,
			VNI:           runnerCfg.VXLAN.VNI,
			UDPPort:       runnerCfg.VXLAN.Port,
			ServerAddress: serverWG,
			ClientAddress: clientWG,
		},
		Network: NetworkResponse{
			GuestMAC:    runnerCfg.Guest.MAC,
			GuestIPv4:   runnerCfg.Guest.IPv4,
			SubnetMask:  runnerCfg.Guest.SubnetMask,
			GatewayIPv4: runnerCfg.Guest.Gateway,
			DNS:         runnerCfg.Guest.DNS,
		},
		Redfish: map[string]string{
			"url":                    redfishURL,
			"username":               runnerCfg.Redfish.Username,
			"password":               runnerCfg.Redfish.Password,
			"certPEM":                pod.Annotations[AnnotationRedfishCertPEM],
			"deviceID":               runnerCfg.Redfish.DeviceID,
			"systemURL":              redfishURL + "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID,
			"serialConsoleStreamURI": "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream",
		},
	}, nil
}

func normalizeArchitecture(value string) (string, error) {
	switch arch := strings.ToLower(strings.TrimSpace(value)); arch {
	case "", ArchitectureAMD64:
		return ArchitectureAMD64, nil
	case ArchitectureARM64:
		return ArchitectureARM64, nil
	default:
		return "", fmt.Errorf("architecture must be %q or %q", ArchitectureAMD64, ArchitectureARM64)
	}
}

func podArchitecture(pod *corev1.Pod) string {
	arch, err := normalizeArchitecture(pod.Labels[LabelArchitecture])
	if err != nil {
		return pod.Labels[LabelArchitecture]
	}

	return arch
}

func serverWireGuardPublicKeyForPod(pod *corev1.Pod) (string, error) {
	serverPublicKey := strings.TrimSpace(pod.Annotations[AnnotationServerWireGuardPublicKey])
	if _, err := wgtypes.ParseKey(serverPublicKey); err != nil {
		return "", fmt.Errorf("runner pod %q has no valid server WireGuard public key", pod.Name)
	}

	return serverPublicKey, nil
}

func (o *Operator) gatewayForPod(ctx context.Context, nodeName string) (string, string, error) {
	if nodeName == "" {
		return "", "", fmt.Errorf("runner pod is not scheduled")
	}

	node := &corev1.Node{}
	if err := o.Client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return "", "", err
	}

	nodePublicIP := nodeExternalIP(node)
	if nodePublicIP != "" {
		return nodePublicIP, nodePublicIP, nil
	}

	gatewayIP, err := o.randomNodeExternalIP(ctx)
	if err != nil {
		return "", "", err
	}

	return gatewayIP, "", nil
}

func (o *Operator) randomNodeExternalIP(ctx context.Context) (string, error) {
	nodes := &corev1.NodeList{}
	if err := o.Client.List(ctx, nodes); err != nil {
		return "", err
	}

	var gatewayIPs []string

	for i := range nodes.Items {
		if ip := nodeExternalIP(&nodes.Items[i]); ip != "" {
			gatewayIPs = append(gatewayIPs, ip)
		}
	}

	if len(gatewayIPs) == 0 {
		return "", fmt.Errorf("no nodes have ExternalIP addresses")
	}

	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(gatewayIPs))))
	if err != nil {
		return "", err
	}

	return gatewayIPs[index.Int64()], nil
}

func nodeExternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && addr.Address != "" {
			return addr.Address
		}
	}

	return ""
}

func (o *Operator) deleteStaleServices(ctx context.Context, activePods map[string]struct{}) error {
	services := &corev1.ServiceList{}
	if err := o.Client.List(ctx, services, client.InNamespace(o.Config.RunnerNamespace), client.MatchingLabels{"app.kubernetes.io/component": "runner-nodeport"}); err != nil {
		return err
	}

	for i := range services.Items {
		service := &services.Items[i]

		podName := service.Labels["playpen.unbounded-cloud.io/pod"]
		if _, ok := activePods[podName]; ok {
			continue
		}

		if err := o.Client.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func (o *Operator) deleteRunnerPod(ctx context.Context, pod *corev1.Pod) error {
	service := &corev1.Service{}

	serviceKey := types.NamespacedName{Namespace: pod.Namespace, Name: serviceNameForPod(pod)}
	if err := o.Client.Get(ctx, serviceKey, service); err == nil {
		if err := o.Client.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	if err := o.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func (o *Operator) claimExpired(pod *corev1.Pod, now time.Time) bool {
	if o.Config.PlaypenTTL <= 0 || pod.Annotations[AnnotationIdempotencyKeyHash] == "" {
		return false
	}

	claimedAt, err := time.Parse(time.RFC3339, pod.Annotations[AnnotationClaimedAt])
	if err != nil {
		return true
	}

	return !claimedAt.Add(o.Config.PlaypenTTL).After(now)
}

func selfSignedCert(commonName string, hosts []string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	notBefore := time.Now().Add(-time.Minute)
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(notBefore.UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM, nil
}

func allocationIDForPod(pod *corev1.Pod) string {
	return hashString(pod.Namespace + "/" + pod.Name)[:16]
}

func serviceNameForPod(pod *corev1.Pod) string {
	return "playpen-runner-" + hashString(pod.Namespace + "/" + pod.Name)[:16]
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}

func runnerAddressIP(value string) (string, error) {
	if i := strings.Index(value, "/"); i >= 0 {
		value = value[:i]
	}

	if net.ParseIP(value) == nil {
		return "", fmt.Errorf("invalid IP address %q", value)
	}

	return value, nil
}

func defaultRunnerPublicRedfishURL(cfg runner.Config) string {
	host, err := runnerAddressIP(cfg.WireGuard.ServerAddress)
	if err != nil {
		return "https://" + cfg.ListenAddr
	}

	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		port = "8443"
	}

	return "https://" + net.JoinHostPort(host, port)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value) //nolint:errcheck // Response body write errors cannot be reported to the client.
}
