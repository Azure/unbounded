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
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	apiregclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"

	apiGroup        = "playpen.unbounded-cloud.io"
	apiVersion      = "v1alpha1"
	apiGroupVersion = apiGroup + "/" + apiVersion
	apiServiceName  = apiVersion + "." + apiGroup

	aggregatedAPIGroupPath   = "/apis/" + apiGroup
	aggregatedAPIVersionPath = aggregatedAPIGroupPath + "/" + apiVersion
	allocationsPath          = aggregatedAPIVersionPath + "/allocations"
	deallocationsPath        = aggregatedAPIVersionPath + "/deallocations"
	allocsPath               = aggregatedAPIVersionPath + "/allocs"
	deallocsPath             = aggregatedAPIVersionPath + "/deallocs"

	extensionAuthNamespace       = "kube-system"
	extensionAuthConfigMapName   = "extension-apiserver-authentication"
	extensionAuthClientCAKey     = "requestheader-client-ca-file"
	extensionAuthAllowedNamesKey = "requestheader-allowed-names"
	remoteUserHeader             = "X-Remote-User"
	remoteGroupHeader            = "X-Remote-Group"

	labelOwned      = "playpen.unbounded-cloud.io/owned"
	labelAllocation = "playpen.unbounded-cloud.io/allocation"
	labelArch       = "playpen.unbounded-cloud.io/architecture"

	annotationExpiresAt = "playpen.unbounded-cloud.io/expires-at"
	annotationRequest   = "playpen.unbounded-cloud.io/request-hash"
)

var (
	virtualMachinesGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	nadsGVR            = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}
)

type Operator struct {
	Client       client.Client
	KubeClient   kubernetes.Interface
	Dynamic      dynamic.Interface
	APIRegClient apiregclient.Interface
	RESTConfig   *rest.Config
	Config       Config
	Scheme       *runtime.Scheme

	aggregatedMu               sync.RWMutex
	aggregatedClientCAs        *x509.CertPool
	aggregatedClientAllowedCNs map[string]struct{}
}

type AllocRequest struct {
	IdempotencyKey     string `json:"idempotencyKey,omitempty"`
	WireGuardPublicKey string `json:"wireGuardPublicKey,omitempty"`
	Architecture       string `json:"architecture,omitempty"`
	DiskSize           string `json:"diskSize,omitempty"`
	Memory             string `json:"memory,omitempty"`
	CPUs               int    `json:"cpus,omitempty"`
}

type DeallocRequest struct {
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	AllocationID   string `json:"allocationID,omitempty"`
}

type AllocResponse struct {
	ID           string            `json:"id"`
	Architecture string            `json:"architecture"`
	ExpiresAt    time.Time         `json:"expiresAt"`
	Objects      map[string]string `json:"objects"`
	Endpoint     EndpointResponse  `json:"endpoint"`
	WireGuard    WireGuardResponse `json:"wireGuard"`
	VXLAN        VXLANResponse     `json:"vxlan"`
	Network      NetworkResponse   `json:"network"`
	Redfish      map[string]string `json:"redfish"`
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
	go func() { errCh <- server.ListenAndServeTLS("", "") }()

	ticker := time.NewTicker(o.Config.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			server.Shutdown(shutdownCtx) //nolint:errcheck

			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
			o.refreshAggregatedClientCAs(ctx)
			o.Reconcile(ctx) //nolint:errcheck
		}
	}
}

func (o *Operator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(aggregatedAPIGroupPath, o.handleAPIGroupDiscovery)
	mux.HandleFunc(aggregatedAPIVersionPath, o.handleAPIVersionDiscovery)
	mux.HandleFunc(allocationsPath, o.handleAllocs)
	mux.HandleFunc(deallocationsPath, o.handleDeallocs)
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
			{"name": "allocations", "singularName": "allocation", "namespaced": false, "kind": "Allocation", "verbs": []string{"create", "get"}},
			{"name": "deallocations", "singularName": "deallocation", "namespaced": false, "kind": "Deallocation", "verbs": []string{"create"}},
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

	resourceName := "allocations"
	if strings.HasSuffix(r.URL.Path, "/allocs") {
		resourceName = "allocs"
	}

	if !o.authorizeAggregatedRequest(w, r, resourceName) {
		return
	}

	var req AllocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)

		return
	}

	idempotencyKey := requestIdempotencyKey(r, req.IdempotencyKey)
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header or idempotencyKey body field is required", http.StatusBadRequest)

		return
	}

	if _, err := wgtypes.ParseKey(strings.TrimSpace(req.WireGuardPublicKey)); err != nil {
		http.Error(w, "wireGuardPublicKey must be a valid WireGuard public key", http.StatusBadRequest)

		return
	}

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

	resourceName := "deallocations"
	if strings.HasSuffix(r.URL.Path, "/deallocs") {
		resourceName = "deallocs"
	}

	if !o.authorizeAggregatedRequest(w, r, resourceName) {
		return
	}

	var req DeallocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)

		return
	}

	id := strings.TrimSpace(req.AllocationID)
	if id == "" {
		id = allocationID(requestIdempotencyKey(r, req.IdempotencyKey))
	}

	if id == "" {
		http.Error(w, "allocationID or Idempotency-Key is required", http.StatusBadRequest)

		return
	}

	status, err := o.Dealloc(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (o *Operator) Alloc(ctx context.Context, idempotencyKey string, req AllocRequest) (AllocResponse, int, error) {
	if o.Dynamic == nil {
		return AllocResponse{}, http.StatusInternalServerError, fmt.Errorf("dynamic client is not configured")
	}

	arch, err := normalizeArchitecture(req.Architecture)
	if err != nil {
		return AllocResponse{}, http.StatusBadRequest, err
	}

	id := allocationID(idempotencyKey)
	name := allocationName(id)
	requestHash := hashRequest(req)
	cm := &corev1.ConfigMap{}
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: o.Config.Namespace, Name: name}, cm); err == nil {
		if cm.Annotations[annotationRequest] != requestHash {
			return AllocResponse{}, http.StatusConflict, fmt.Errorf("idempotency key was already used with a different request")
		}

		var resp AllocResponse
		if err := json.Unmarshal([]byte(cm.Data["response.json"]), &resp); err != nil {
			return AllocResponse{}, http.StatusInternalServerError, err
		}

		return resp, http.StatusOK, nil
	} else if !apierrors.IsNotFound(err) {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	serverKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	redfishPassword, err := randomHex(18)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	expiresAt := time.Now().UTC().Add(o.Config.AllocationTTL)
	hostPort, err := o.allocateHostPort(ctx, name)
	if err != nil {
		return AllocResponse{}, http.StatusServiceUnavailable, err
	}

	params := allocationParams(id, hostPort, o.Config)
	redfishCert, redfishKey, err := selfSignedCert(name, []string{name, "localhost", params.serverWG})
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	resp := AllocResponse{
		ID:           id,
		Architecture: arch,
		ExpiresAt:    expiresAt,
		Objects: map[string]string{
			"namespace": o.Config.Namespace,
			"vm":        name,
			"endpoint":  name + "-endpoint",
			"secret":    name,
		},
		Endpoint: EndpointResponse{Host: "", WireGuardUDPPort: hostPort, ExternalTrafficPolicy: "Local"},
		WireGuard: WireGuardResponse{
			Interface:       "wg0",
			ServerPublicKey: serverKey.PublicKey().String(),
			ServerAddress:   params.serverWG + "/24",
			ClientAddress:   params.clientWG + "/24",
			ListenPort:      o.Config.EndpointListenPort,
		},
		VXLAN: VXLANResponse{
			Interface:     "vxlan0",
			VNI:           params.vni,
			UDPPort:       o.Config.VXLANPort,
			ServerAddress: params.serverWG,
			ClientAddress: params.clientWG,
		},
		Network: NetworkResponse{
			GuestMAC:    params.mac,
			GuestIPv4:   params.guestIP,
			SubnetMask:  "255.255.255.0",
			GatewayIPv4: params.gatewayIP,
			DNS:         o.Config.GuestDNS,
		},
		Redfish: map[string]string{
			"url":                    "https://" + params.serverWG + ":" + strconv.Itoa(o.Config.RedfishPort),
			"username":               "playpen",
			"password":               redfishPassword,
			"deviceID":               "1",
			"certPEM":                string(redfishCert),
			"systemURL":              "https://" + params.serverWG + ":" + strconv.Itoa(o.Config.RedfishPort) + "/redfish/v1/Systems/1",
			"serialConsoleStreamURI": "/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream",
		},
	}

	labels := allocationLabels(id, arch)
	annotations := map[string]string{annotationExpiresAt: expiresAt.Format(time.RFC3339), annotationRequest: requestHash}
	if err := o.createSecret(ctx, name, labels, annotations, serverKey.String(), redfishPassword, redfishCert, redfishKey); err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	if err := o.createNADs(ctx, name, labels, annotations, params.bridge); err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	if err := o.createVM(ctx, name, arch, req, labels, annotations, params); err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	if err := o.createEndpointPod(ctx, name, arch, req.WireGuardPublicKey, labels, annotations, params, hostPort); err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	if err := o.fillEndpointHost(ctx, &resp, name); err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Config.Namespace, Labels: labels, Annotations: annotations},
		Data:       map[string]string{"response.json": string(data)},
	}
	if err := o.Client.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	return resp, http.StatusOK, nil
}

func (o *Operator) Dealloc(ctx context.Context, id string) (int, error) {
	name := allocationName(id)
	if name == "" {
		return http.StatusBadRequest, fmt.Errorf("invalid allocation id")
	}

	if err := o.deleteAllocation(ctx, name); err != nil {
		return http.StatusInternalServerError, err
	}

	return http.StatusNoContent, nil
}

func (o *Operator) Reconcile(ctx context.Context) error {
	list := &corev1.ConfigMapList{}
	if err := o.Client.List(ctx, list, client.InNamespace(o.Config.Namespace), client.MatchingLabels{labelOwned: "true"}); err != nil {
		return err
	}

	now := time.Now()
	for _, cm := range list.Items {
		expiresAt, err := time.Parse(time.RFC3339, cm.Annotations[annotationExpiresAt])
		if err != nil || now.After(expiresAt) {
			if err := o.deleteAllocation(ctx, cm.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

func (o *Operator) createSecret(ctx context.Context, name string, labels, annotations map[string]string, wgKey, redfishPassword string, redfishCert, redfishKey []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Config.Namespace, Labels: labels, Annotations: annotations},
		Data: map[string][]byte{
			"wireguard-private-key": []byte(wgKey),
			"redfish-password":      []byte(redfishPassword),
			"tls.crt":               redfishCert,
			"tls.key":               redfishKey,
		},
	}

	return ignoreAlreadyExists(o.Client.Create(ctx, secret))
}

func (o *Operator) createNADs(ctx context.Context, name string, labels, annotations map[string]string, bridge string) error {
	for _, item := range []struct {
		name string
		vm   bool
	}{
		{name: name + "-vm", vm: true},
		{name: name + "-pod"},
	} {
		config := map[string]any{
			"cniVersion": "1.0.0",
			"name":       item.name,
			"type":       "bridge",
			"bridge":     bridge,
			"mtu":        1400,
			"ipam":       map[string]any{},
		}
		if item.vm {
			config["disableContainerInterface"] = true
			config["macspoofchk"] = true
		}

		data, err := json.Marshal(config)
		if err != nil {
			return err
		}

		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":        item.name,
				"namespace":   o.Config.Namespace,
				"labels":      labels,
				"annotations": annotations,
			},
			"spec": map[string]any{"config": string(data)},
		}}
		if _, err := o.Dynamic.Resource(nadsGVR).Namespace(o.Config.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}

func (o *Operator) createVM(ctx context.Context, name, arch string, req AllocRequest, labels, annotations map[string]string, params allocationParameters) error {
	diskSize := strings.TrimSpace(req.DiskSize)
	if diskSize == "" {
		diskSize = o.Config.DefaultDiskSize
	}

	memory := strings.TrimSpace(req.Memory)
	if memory == "" {
		memory = o.Config.DefaultMemory
	}

	cpus := req.CPUs
	if cpus <= 0 {
		cpus = o.Config.DefaultCPUs
	}

	firmware := map[string]any{"bootloader": map[string]any{"efi": map[string]any{"secureBoot": false}}}
	machine := map[string]any{}
	if arch == ArchitectureARM64 {
		machine["type"] = "virt"
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   o.Config.Namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"runStrategy": "Manual",
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels, "annotations": annotations},
				"spec": map[string]any{
					"nodeSelector": map[string]any{"kubernetes.io/arch": arch},
					"domain": map[string]any{
						"cpu":       map[string]any{"cores": cpus},
						"firmware":  firmware,
						"machine":   machine,
						"resources": map[string]any{"requests": map[string]any{"memory": memory}},
						"devices": map[string]any{
							"interfaces": []any{
								map[string]any{"name": "default", "masquerade": map[string]any{}},
								map[string]any{"name": "pxe", "bridge": map[string]any{}, "model": "e1000", "macAddress": params.mac, "bootOrder": 1},
							},
							"disks": []any{
								map[string]any{"name": "rootdisk", "disk": map[string]any{"bus": "virtio"}, "bootOrder": 2},
							},
						},
					},
					"networks": []any{
						map[string]any{"name": "default", "pod": map[string]any{}},
						map[string]any{"name": "pxe", "multus": map[string]any{"networkName": name + "-vm"}},
					},
					"volumes": []any{
						map[string]any{"name": "rootdisk", "emptyDisk": map[string]any{"capacity": diskSize}},
					},
				},
			},
		},
	}}

	_, err := o.Dynamic.Resource(virtualMachinesGVR).Namespace(o.Config.Namespace).Create(ctx, obj, metav1.CreateOptions{})

	return ignoreAlreadyExists(err)
}

func (o *Operator) createEndpointPod(ctx context.Context, name, arch, clientPubKey string, labels, annotations map[string]string, params allocationParameters, hostPort int32) error {
	privileged := true
	addCaps := []corev1.Capability{"NET_ADMIN", "NET_RAW"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name + "-endpoint",
			Namespace:   o.Config.Namespace,
			Labels:      labels,
			Annotations: copyMap(annotations, map[string]string{"k8s.v1.cni.cncf.io/networks": `[{"name":"` + name + `-pod","interface":"pxe0"}]`}),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: o.Config.ServiceAccount,
			NodeSelector:       map[string]string{"kubernetes.io/arch": arch},
			RestartPolicy:      corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:            "endpoint",
				Image:           o.Config.Image,
				ImagePullPolicy: corev1.PullPolicy(o.Config.ImagePullPolicy),
				Command:         []string{"/unbounded/bin/playpen-endpoint"},
				Env: []corev1.EnvVar{
					{Name: "PLAYPEN_NAMESPACE", Value: o.Config.Namespace},
					{Name: "PLAYPEN_VM_NAME", Value: name},
					{Name: "PLAYPEN_DEVICE_ID", Value: "1"},
					{Name: "PLAYPEN_REDFISH_USERNAME", Value: "playpen"},
					{Name: "PLAYPEN_REDFISH_PASSWORD_FILE", Value: "/etc/playpen/secret/redfish-password"},
					{Name: "PLAYPEN_WG_PRIVATE_KEY_FILE", Value: "/etc/playpen/secret/wireguard-private-key"},
					{Name: "PLAYPEN_WG_ADDRESS", Value: params.serverWG + "/24"},
					{Name: "PLAYPEN_WG_PEER_PUBLIC_KEY", Value: strings.TrimSpace(clientPubKey)},
					{Name: "PLAYPEN_WG_PEER_ADDRESS", Value: params.clientWG + "/32"},
					{Name: "PLAYPEN_WG_LISTEN_PORT", Value: strconv.Itoa(o.Config.EndpointListenPort)},
					{Name: "PLAYPEN_VXLAN_VNI", Value: strconv.Itoa(params.vni)},
					{Name: "PLAYPEN_VXLAN_PORT", Value: strconv.Itoa(o.Config.VXLANPort)},
					{Name: "PLAYPEN_VXLAN_LOCAL", Value: params.serverWG},
					{Name: "PLAYPEN_VXLAN_REMOTE", Value: params.clientWG},
					{Name: "PLAYPEN_REDFISH_ADDR", Value: params.serverWG + ":" + strconv.Itoa(o.Config.RedfishPort)},
					{Name: "PLAYPEN_TLS_CERT", Value: "/etc/playpen/secret/tls.crt"},
					{Name: "PLAYPEN_TLS_KEY", Value: "/etc/playpen/secret/tls.key"},
				},
				Ports: []corev1.ContainerPort{{Name: "wireguard", ContainerPort: int32(o.Config.EndpointListenPort), HostPort: hostPort, Protocol: corev1.ProtocolUDP}},
				SecurityContext: &corev1.SecurityContext{
					Privileged:   &privileged,
					Capabilities: &corev1.Capabilities{Add: addCaps},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "secret", MountPath: "/etc/playpen/secret", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}}}},
		},
	}

	return ignoreAlreadyExists(o.Client.Create(ctx, pod))
}

func (o *Operator) fillEndpointHost(ctx context.Context, resp *AllocResponse, name string) error {
	host, err := o.waitForEndpointHost(ctx, name)
	if err != nil {
		return err
	}

	resp.Endpoint.Host = host

	return nil
}

func (o *Operator) waitForEndpointHost(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		host, err := o.endpointHost(ctx, name)
		if err == nil && host != "" {
			return host, nil
		}

		if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("waiting for endpoint host: %w", lastErr)
			}

			return "", fmt.Errorf("waiting for endpoint host: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (o *Operator) endpointHost(ctx context.Context, name string) (string, error) {
	pod := &corev1.Pod{}
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: o.Config.Namespace, Name: name + "-endpoint"}, pod); err != nil {
		return "", err
	}

	if pod.Spec.NodeName == "" {
		return "", nil
	}

	node := &corev1.Node{}
	if err := o.Client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		return "", err
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}

	return "", fmt.Errorf("node %s has no usable address", node.Name)
}

func (o *Operator) allocateHostPort(ctx context.Context, name string) (int32, error) {
	pods := &corev1.PodList{}
	if err := o.Client.List(ctx, pods, client.InNamespace(o.Config.Namespace), client.MatchingLabels{labelOwned: "true"}); err != nil {
		return 0, err
	}

	used := map[int32]struct{}{}
	for _, pod := range pods.Items {
		if pod.Name == name+"-endpoint" {
			for _, c := range pod.Spec.Containers {
				for _, p := range c.Ports {
					if p.Name == "wireguard" && p.HostPort != 0 {
						return p.HostPort, nil
					}
				}
			}
		}

		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.HostPort != 0 {
					used[p.HostPort] = struct{}{}
				}
			}
		}
	}

	for port := o.Config.WireGuardHostPortStart; port <= o.Config.WireGuardHostPortEnd; port++ {
		if _, ok := used[port]; !ok {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no free WireGuard hostPort in range %d-%d", o.Config.WireGuardHostPortStart, o.Config.WireGuardHostPortEnd)
}

func (o *Operator) deleteAllocation(ctx context.Context, name string) error {
	_ = o.Dynamic.Resource(virtualMachinesGVR).Namespace(o.Config.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = o.Dynamic.Resource(nadsGVR).Namespace(o.Config.Namespace).Delete(ctx, name+"-vm", metav1.DeleteOptions{})
	_ = o.Dynamic.Resource(nadsGVR).Namespace(o.Config.Namespace).Delete(ctx, name+"-pod", metav1.DeleteOptions{})
	_ = o.Client.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-endpoint", Namespace: o.Config.Namespace}})
	_ = o.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Config.Namespace}})
	_ = o.Client.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Config.Namespace}})

	return nil
}

type allocationParameters struct {
	serverWG  string
	clientWG  string
	vni       int
	bridge    string
	mac       string
	guestIP   string
	gatewayIP string
}

func allocationParams(id string, _ int32, cfg Config) allocationParameters {
	sum := sha256.Sum256([]byte(id))
	third := int(sum[0])
	if third == 0 || third == 255 {
		third = 42
	}

	guestOctet := int(sum[1])
	if guestOctet == 0 || guestOctet == 255 {
		guestOctet = 200
	}

	return allocationParameters{
		serverWG:  fmt.Sprintf("10.250.%d.1", third),
		clientWG:  fmt.Sprintf("10.250.%d.2", third),
		vni:       10000 + int(binaryMod(sum[:], 50000)),
		bridge:    "br" + id[:10],
		mac:       fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[2], sum[3], sum[4], sum[5], sum[6]),
		gatewayIP: fmt.Sprintf("192.168.%d.1", guestOctet),
		guestIP:   fmt.Sprintf("192.168.%d.10", guestOctet),
	}
}

func allocationID(idempotencyKey string) string {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:])[:16]
}

func allocationName(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}

	return "pp-" + strings.TrimSpace(id)
}

func allocationLabels(id, arch string) map[string]string {
	return map[string]string{labelOwned: "true", labelAllocation: id, labelArch: arch}
}

func normalizeArchitecture(arch string) (string, error) {
	switch strings.TrimSpace(arch) {
	case "", ArchitectureAMD64:
		return ArchitectureAMD64, nil
	case ArchitectureARM64:
		return ArchitectureARM64, nil
	default:
		return "", fmt.Errorf("unsupported architecture %q", arch)
	}
}

func hashRequest(req AllocRequest) string {
	req.IdempotencyKey = ""
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func binaryMod(data []byte, mod uint64) uint64 {
	var value uint64
	for _, b := range data[:8] {
		value = (value << 8) | uint64(b)
	}

	return value % mod
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

func copyMap(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}

	for k, v := range extra {
		out[k] = v
	}

	return out
}

func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	return err
}

func requestIdempotencyKey(r *http.Request, bodyValue string) string {
	if value := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); value != "" {
		return value
	}

	return strings.TrimSpace(bodyValue)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintln(w, err.Error()) //nolint:errcheck
	}
}

func (o *Operator) EnsureTLSSecret(ctx context.Context) (tls.Certificate, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}
	if err := o.Client.Get(ctx, key, secret); err == nil {
		return tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	} else if !apierrors.IsNotFound(err) {
		return tls.Certificate{}, err
	}

	certPEM, keyPEM, err := selfSignedCert(o.Config.ServiceName, []string{
		o.Config.ServiceName,
		o.Config.ServiceName + "." + o.Config.Namespace,
		o.Config.ServiceName + "." + o.Config.Namespace + ".svc",
		o.Config.ServiceName + "." + o.Config.Namespace + ".svc.cluster.local",
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
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}, secret); err != nil {
		return nil, err
	}

	return secret.Data[corev1.TLSCertKey], nil
}

func (o *Operator) injectAPIServiceCABundle(ctx context.Context, caBundle []byte) error {
	if o.APIRegClient == nil || len(caBundle) == 0 {
		return nil
	}

	apiService, err := o.APIRegClient.ApiregistrationV1().APIServices().Get(ctx, apiServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	if bytes.Equal(apiService.Spec.CABundle, caBundle) {
		return nil
	}

	apiService.Spec.CABundle = caBundle
	_, err = o.APIRegClient.ApiregistrationV1().APIServices().Update(ctx, apiService, metav1.UpdateOptions{})

	return err
}

func (o *Operator) refreshAggregatedClientCAs(ctx context.Context) {
	if o.KubeClient == nil {
		return
	}

	cm, err := o.KubeClient.CoreV1().ConfigMaps(extensionAuthNamespace).Get(ctx, extensionAuthConfigMapName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to read %s/%s: %v", extensionAuthNamespace, extensionAuthConfigMapName, err)

		return
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cm.Data[extensionAuthClientCAKey])) {
		return
	}

	allowed, err := parseRequestHeaderAllowedNames(cm.Data[extensionAuthAllowedNamesKey])
	if err != nil {
		return
	}

	o.aggregatedMu.Lock()
	defer o.aggregatedMu.Unlock()
	o.aggregatedClientCAs = pool
	o.aggregatedClientAllowedCNs = allowed
}

func parseRequestHeaderAllowedNames(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}

	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, err
	}

	allowed := map[string]struct{}{}
	for _, name := range names {
		if name != "" {
			allowed[name] = struct{}{}
		}
	}

	return allowed, nil
}

func (o *Operator) authorizeAggregatedRequest(w http.ResponseWriter, r *http.Request, resource string) bool {
	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	user := strings.TrimSpace(r.Header.Get(remoteUserHeader))
	if user == "" {
		http.Error(w, "missing remote user", http.StatusForbidden)

		return false
	}

	sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User:   user,
		Groups: remoteGroups(r),
		ResourceAttributes: &authzv1.ResourceAttributes{
			Verb:     "create",
			Group:    apiGroup,
			Resource: resource,
		},
	}}

	result, err := o.KubeClient.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), sar, metav1.CreateOptions{})
	if err != nil || !result.Status.Allowed || result.Status.Denied {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	return true
}

func remoteGroups(r *http.Request) []string {
	var groups []string
	for _, value := range r.Header.Values(remoteGroupHeader) {
		for _, group := range strings.Split(value, ",") {
			if group = strings.TrimSpace(group); group != "" {
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

	if _, err := leaf.Verify(x509.VerifyOptions{Roots: clientCAs, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return false
	}

	if len(allowedCNs) == 0 {
		return true
	}

	_, ok := allowedCNs[leaf.Subject.CommonName]

	return ok
}

func selfSignedCert(commonName string, dnsNames []string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}

	for _, name := range dnsNames {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM, nil
}
