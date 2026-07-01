// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const idempotencyKeyHeader = "Idempotency-Key"

type Operator struct {
	Client client.Client
	Config Config
	Scheme *runtime.Scheme
}

type ClaimRequest struct {
	WireGuardPublicKey string `json:"wireGuardPublicKey"`
}

type ClaimResponse struct {
	Pod       PodResponse       `json:"pod"`
	Endpoint  EndpointResponse  `json:"endpoint"`
	WireGuard WireGuardResponse `json:"wireGuard"`
	VXLAN     VXLANResponse     `json:"vxlan"`
	Network   NetworkResponse   `json:"network"`
	Redfish   map[string]string `json:"redfish"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type PodResponse struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	NodeName     string `json:"nodeName"`
	NodePublicIP string `json:"nodePublicIP"`
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

	server := &http.Server{
		Addr:              o.Config.ListenAddr,
		Handler:           o.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServeTLS("", "")
	}()

	ticker := time.NewTicker(o.Config.ReconcileInterval)
	defer ticker.Stop()
	_ = o.ReconcileRunners(ctx)

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)

			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
			_ = o.ReconcileRunners(ctx)
		}
	}
}

func (o *Operator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/playpen/v1/claims", o.handleClaims)
	mux.HandleFunc("/playpen/v1/releases", o.handleReleases)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	return mux
}

func (o *Operator) handleClaims(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)

		return
	}

	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)

		return
	}

	req.WireGuardPublicKey = strings.TrimSpace(req.WireGuardPublicKey)
	if _, err := wgtypes.ParseKey(req.WireGuardPublicKey); err != nil {
		http.Error(w, "wireGuardPublicKey must be a valid WireGuard public key", http.StatusBadRequest)

		return
	}

	resp, status, err := o.Claim(r.Context(), idempotencyKey, req)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (o *Operator) handleReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)

		return
	}

	status, err := o.Release(r.Context(), idempotencyKey)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (o *Operator) Claim(ctx context.Context, idempotencyKey string, req ClaimRequest) (ClaimResponse, int, error) {
	keyHash := hashString(idempotencyKey)
	reqHash := hashString(req.WireGuardPublicKey)

	pods, err := o.listRunnerPods(ctx)
	if err != nil {
		return ClaimResponse{}, http.StatusInternalServerError, err
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != keyHash {
			continue
		}

		if pod.Annotations[AnnotationRequestHash] != reqHash {
			return ClaimResponse{}, http.StatusConflict, fmt.Errorf("idempotency key was already used with a different request")
		}

		resp, err := o.responseForPod(ctx, pod)
		if err != nil {
			return ClaimResponse{}, http.StatusServiceUnavailable, err
		}

		return resp, http.StatusOK, nil
	}

	var unavailable []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
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
				return ClaimResponse{}, http.StatusConflict, err
			}

			return ClaimResponse{}, http.StatusInternalServerError, err
		}
		if !claimed {
			resp, err := o.responseForPod(ctx, allocatedPod)
			if err != nil {
				return ClaimResponse{}, http.StatusServiceUnavailable, err
			}

			return resp, http.StatusOK, nil
		}

		service, err := o.ensureNodePortService(ctx, allocatedPod)
		if err != nil {
			return ClaimResponse{}, http.StatusInternalServerError, err
		}

		return o.buildResponse(allocatedPod, gatewayIP, nodePublicIP, service, serverPublicKey), http.StatusOK, nil
	}

	if len(unavailable) > 0 {
		return ClaimResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available with an ExternalIP gateway: %s", strings.Join(unavailable, "; "))
	}

	return ClaimResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available")
}

func (o *Operator) Release(ctx context.Context, idempotencyKey string) (int, error) {
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

	certPEM, keyPEM, err := selfSignedCert("playpen-operator", []string{"playpen-operator", "playpen-operator." + o.Config.Namespace, "localhost"})
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

func (o *Operator) responseForPod(ctx context.Context, pod *corev1.Pod) (ClaimResponse, error) {
	serverPublicKey, err := serverWireGuardPublicKeyForPod(pod)
	if err != nil {
		return ClaimResponse{}, err
	}

	gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod.Spec.NodeName)
	if err != nil {
		return ClaimResponse{}, err
	}

	service, err := o.ensureNodePortService(ctx, pod)
	if err != nil {
		return ClaimResponse{}, err
	}

	return o.buildResponse(pod, gatewayIP, nodePublicIP, service, serverPublicKey), nil
}

func (o *Operator) ensureNodePortService(ctx context.Context, pod *corev1.Pod) (*corev1.Service, error) {
	name := serviceNameForPod(pod)
	service := &corev1.Service{}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: name}
	if err := o.Client.Get(ctx, key, service); err == nil {
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			base := service.DeepCopy()
			service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
			if err := o.Client.Patch(ctx, service, client.MergeFrom(base)); err != nil {
				return nil, err
			}
		}

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

func (o *Operator) buildResponse(pod *corev1.Pod, gatewayIP, nodePublicIP string, service *corev1.Service, serverPublicKey string) ClaimResponse {
	runnerCfg := o.Config.Runner
	nodePort := int32(0)
	if len(service.Spec.Ports) > 0 {
		nodePort = service.Spec.Ports[0].NodePort
	}

	serverWG, _ := runnerAddressIP(runnerCfg.WireGuard.ServerAddress)
	clientWG, _ := runnerAddressIP(runnerCfg.WireGuard.ClientAddress)
	redfishURL := runnerCfg.PublicRedfishURL
	if redfishURL == "" {
		redfishURL = "https://" + runnerCfg.ListenAddr
	}

	return ClaimResponse{
		Pod: PodResponse{
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			NodeName:     pod.Spec.NodeName,
			NodePublicIP: nodePublicIP,
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
			"deviceID":               runnerCfg.Redfish.DeviceID,
			"systemURL":              redfishURL + "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID,
			"serialConsoleStreamURI": "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream",
		},
		Metadata: map[string]string{
			"serviceName": service.Name,
		},
	}
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
		SerialNumber: big.NewInt(notBefore.UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
