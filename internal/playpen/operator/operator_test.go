// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestAllocAllocatesRunnerWithHostPortEndpoint(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}

	resp, status, err := op.Alloc(t.Context(), "alloc-key", req)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}

	if resp.Endpoint.Host != "20.30.40.50" {
		t.Fatalf("endpoint host = %q, want node external ip", resp.Endpoint.Host)
	}

	if resp.Pod.NodePublicIP != "20.30.40.50" {
		t.Fatalf("pod node public ip = %q, want node external ip", resp.Pod.NodePublicIP)
	}

	if resp.Pod.Architecture != ArchitectureAMD64 {
		t.Fatalf("pod architecture = %q, want %q", resp.Pod.Architecture, ArchitectureAMD64)
	}

	if resp.Endpoint.WireGuardUDPPort == 0 {
		t.Fatal("endpoint WireGuard UDP port is empty")
	}

	if resp.Endpoint.ExternalTrafficPolicy != "HostPort" {
		t.Fatalf("externalTrafficPolicy = %q, want HostPort", resp.Endpoint.ExternalTrafficPolicy)
	}

	if resp.WireGuard.ServerPublicKey == "" {
		t.Fatal("server WireGuard public key is empty")
	}

	if resp.WireGuard.ServerPublicKey != podServerPublicKey("runner-1") {
		t.Fatalf("server WireGuard public key = %q, want pod annotation", resp.WireGuard.ServerPublicKey)
	}

	if got, want := resp.Redfish["url"], "https://10.88.0.1:8443"; got != want {
		t.Fatalf("redfish url = %q, want %q", got, want)
	}

	if resp.Redfish["systemURL"] != resp.Redfish["url"]+"/redfish/v1/Systems/"+resp.Redfish["deviceID"] {
		t.Fatalf("redfish system URL = %q", resp.Redfish["systemURL"])
	}

	if resp.Redfish["serialConsoleStreamURI"] != "/redfish/v1/Systems/"+resp.Redfish["deviceID"]+"/Oem/Unbounded/SerialConsole/Stream" {
		t.Fatalf("redfish serial console stream URI = %q", resp.Redfish["serialConsoleStreamURI"])
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}

	if pod.Annotations[AnnotationClientWireGuardPublicKey] != req.WireGuardPublicKey {
		t.Fatalf("pod allocation annotation = %q, want %q", pod.Annotations[AnnotationClientWireGuardPublicKey], req.WireGuardPublicKey)
	}

	if pod.Labels[LabelAllocated] == "" {
		t.Fatal("pod allocation label is empty")
	}
}

func TestAllocPrefersRunnerHostPortOnNodeExternalIP(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithHostPort("runner-1", "node-1", 51821),
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.Endpoint.Host != "20.30.40.50" {
		t.Fatalf("endpoint host = %q, want runner node external IP", resp.Endpoint.Host)
	}

	if got, want := resp.Endpoint.WireGuardUDPPort, int32(51821); got != want {
		t.Fatalf("endpoint port = %d, want host port %d", got, want)
	}

	if resp.Endpoint.ExternalTrafficPolicy != "HostPort" {
		t.Fatalf("externalTrafficPolicy = %q, want HostPort", resp.Endpoint.ExternalTrafficPolicy)
	}
}

func TestAllocUsesDistinctHostPortsForMultiplePodsOnSameNode(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithHostPort("runner-1", "node-1", 51821),
		testPodWithHostPort("runner-2", "node-1", 51822),
	)

	first, status, err := op.Alloc(t.Context(), "alloc-key-1", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("first alloc status=%d err=%v", status, err)
	}

	second, status, err := op.Alloc(t.Context(), "alloc-key-2", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("second alloc status=%d err=%v", status, err)
	}

	if first.Pod.Name == second.Pod.Name {
		t.Fatalf("second alloc reused pod %q", second.Pod.Name)
	}

	if first.Endpoint.Host != "20.30.40.50" || second.Endpoint.Host != "20.30.40.50" {
		t.Fatalf("endpoints used unexpected hosts: first=%q second=%q", first.Endpoint.Host, second.Endpoint.Host)
	}

	if first.Endpoint.WireGuardUDPPort == second.Endpoint.WireGuardUDPPort {
		t.Fatalf("allocs reused host port %d", first.Endpoint.WireGuardUDPPort)
	}
}

func TestAllocIsIdempotentAndConflictsOnDifferentRequest(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}

	first, status, err := op.Alloc(t.Context(), "alloc-key", req)
	if err != nil || status != 200 {
		t.Fatalf("first alloc status=%d err=%v", status, err)
	}

	second, status, err := op.Alloc(t.Context(), "alloc-key", req)
	if err != nil || status != 200 {
		t.Fatalf("second alloc status=%d err=%v", status, err)
	}

	if second.Pod.Name != first.Pod.Name {
		t.Fatalf("second pod = %q, want %q", second.Pod.Name, first.Pod.Name)
	}

	_, status, err = op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("different alloc request with same idempotency key succeeded")
	}

	if status != 409 {
		t.Fatalf("conflict status = %d, want 409", status)
	}
}

func TestAllocDefaultsToAMD64AndSkipsARM64Runner(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithArchitecture("arm64-runner", "node-1", ArchitectureARM64),
		testPodWithArchitecture("amd64-runner", "node-1", ArchitectureAMD64),
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	if resp.Pod.Name != "amd64-runner" {
		t.Fatalf("pod name = %q, want amd64-runner", resp.Pod.Name)
	}

	if resp.Pod.Architecture != ArchitectureAMD64 {
		t.Fatalf("pod architecture = %q, want %q", resp.Pod.Architecture, ArchitectureAMD64)
	}
}

func TestAllocCanRequestARM64Runner(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithArchitecture("amd64-runner", "node-1", ArchitectureAMD64),
		testPodWithArchitecture("arm64-runner", "node-1", ArchitectureARM64),
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t), Architecture: ArchitectureARM64})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	if resp.Pod.Name != "arm64-runner" {
		t.Fatalf("pod name = %q, want arm64-runner", resp.Pod.Name)
	}

	if resp.Pod.Architecture != ArchitectureARM64 {
		t.Fatalf("pod architecture = %q, want %q", resp.Pod.Architecture, ArchitectureARM64)
	}
}

func TestAllocRejectsInvalidArchitecture(t *testing.T) {
	op := testOperator(t)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t), Architecture: "s390x"})
	if err == nil {
		t.Fatal("alloc succeeded with invalid architecture")
	}

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestAllocReportsNoMatchingArchitecture(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithArchitecture("amd64-runner", "node-1", ArchitectureAMD64),
	)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t), Architecture: ArchitectureARM64})
	if err == nil {
		t.Fatal("alloc succeeded without arm64 runner")
	}

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
}

func TestAllocIdempotencyIncludesArchitecture(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithArchitecture("amd64-runner", "node-1", ArchitectureAMD64),
		testPodWithArchitecture("arm64-runner", "node-1", ArchitectureARM64),
	)
	publicKey := testPublicKey(t)

	if _, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: publicKey, Architecture: ArchitectureAMD64}); err != nil || status != http.StatusOK {
		t.Fatalf("first alloc status=%d err=%v", status, err)
	}

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: publicKey, Architecture: ArchitectureARM64})
	if err == nil {
		t.Fatal("same idempotency key succeeded with different architecture")
	}

	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
}

func TestAllocRejectsInvalidResourceType(t *testing.T) {
	op := testOperator(t)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: "database"})
	if err == nil {
		t.Fatal("alloc succeeded with invalid resource type")
	}

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestAllocAllocatesControlPlaneWithHostReachableKubeconfig(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testControlPlanePod("control-plane-1", "node-1", "v1.33.1", 16443),
	)
	op.Config.ControlPlaneVersions = []string{"v1.33.1"}

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: ResourceTypeControlPlane, KubernetesVersion: "1.33.1"})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.ResourceType != ResourceTypeControlPlane || resp.Pod.ResourceType != ResourceTypeControlPlane {
		t.Fatalf("resource type response = %#v", resp)
	}

	if resp.ControlPlane.KubernetesVersion != "v1.33.1" {
		t.Fatalf("version = %q, want v1.33.1", resp.ControlPlane.KubernetesVersion)
	}

	if resp.ControlPlane.APIServerURL != "https://20.30.40.50:16443" {
		t.Fatalf("host API URL = %q", resp.ControlPlane.APIServerURL)
	}

	if resp.ControlPlane.GuestAPIServerURL != "https://192.168.200.1:6443" {
		t.Fatalf("guest API URL = %q", resp.ControlPlane.GuestAPIServerURL)
	}

	cfg, err := clientcmd.Load([]byte(resp.ControlPlane.Kubeconfig))
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}

	for _, cluster := range cfg.Clusters {
		if cluster.Server != "https://20.30.40.50:16443" {
			t.Fatalf("cluster server = %q", cluster.Server)
		}

		if cluster.TLSServerName != "192.168.200.1" {
			t.Fatalf("tls server name = %q", cluster.TLSServerName)
		}
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "control-plane-1"}, pod); err != nil {
		t.Fatal(err)
	}

	if pod.Annotations[AnnotationClientWireGuardPublicKey] != "" {
		t.Fatalf("client public key = %q, want empty", pod.Annotations[AnnotationClientWireGuardPublicKey])
	}

	if pod.Labels[LabelAllocated] == "" {
		t.Fatal("pod allocation label is empty")
	}
}

func TestAllocDefaultsControlPlaneVersion(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testControlPlanePod("control-plane-1", "node-1", "v1.33.0", 16443),
		testControlPlanePod("control-plane-2", "node-1", "v1.34.0", 16444),
	)
	op.Config.ControlPlaneVersions = []string{"v1.33.0", "v1.34.0"}

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: ResourceTypeControlPlane})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	if resp.ControlPlane.KubernetesVersion != "v1.34.0" {
		t.Fatalf("version = %q, want default v1.34.0", resp.ControlPlane.KubernetesVersion)
	}
}

func TestAllocDefaultsControlPlaneVersionToLatestUnorderedVersion(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testControlPlanePod("control-plane-1", "node-1", "v1.34.1", 16443),
	)
	op.Config.ControlPlaneVersions = []string{"v1.34.1", "v1.33.9"}

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: ResourceTypeControlPlane})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	if resp.ControlPlane.KubernetesVersion != "v1.34.1" {
		t.Fatalf("version = %q, want default v1.34.1", resp.ControlPlane.KubernetesVersion)
	}
}

func TestAllocRejectsUnconfiguredControlPlaneVersion(t *testing.T) {
	op := testOperator(t)
	op.Config.ControlPlaneVersions = []string{"v1.33.0"}

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: ResourceTypeControlPlane, KubernetesVersion: "v1.34.0"})
	if err == nil {
		t.Fatal("alloc succeeded with unconfigured version")
	}

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestAllocIdempotencyIncludesResourceType(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
		testControlPlanePod("control-plane-1", "node-1", "v1.33.0", 16443),
	)

	if _, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)}); err != nil || status != http.StatusOK {
		t.Fatalf("runner alloc status=%d err=%v", status, err)
	}

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{ResourceType: ResourceTypeControlPlane})
	if err == nil {
		t.Fatal("same idempotency key succeeded with different resource type")
	}

	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
}

func TestAllocRequiresRunnerNodeExternalIP(t *testing.T) {
	op := testOperator(
		t,
		testNode("private-node", ""),
		testNode("gateway-node", "20.30.40.50"),
		testPod("runner-1", "private-node", nil),
	)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("alloc succeeded when runner node had no ExternalIP")
	}

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}

	if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
		t.Fatal("pod was allocated despite missing runner node ExternalIP")
	}
}

func TestAllocRequiresAnyNodeExternalIP(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", ""),
		testPod("runner-1", "node-1", nil),
	)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("alloc succeeded without any node ExternalIP")
	}

	if status != 503 {
		t.Fatalf("status = %d, want 503", status)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}

	if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
		t.Fatal("pod was allocated despite missing gateway ExternalIP")
	}
}

func TestAllocSkipsRunnerWithoutServerWireGuardPublicKey(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPodWithAnnotations("runner-1", "node-1", map[string]string{}),
	)

	_, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("alloc succeeded without server WireGuard public key")
	}

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}

	if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
		t.Fatal("pod was allocated despite missing server WireGuard public key")
	}
}

func TestAllocSkipsUnavailableRunnerPods(t *testing.T) {
	terminating := testPod("terminating-runner", "node-1", nil)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test.finalizer"}

	pending := testPod("pending-runner", "node-1", nil)
	pending.Status.Phase = corev1.PodPending

	unready := testPod("unready-runner", "node-1", nil)
	unready.Status.ContainerStatuses[0].Ready = false

	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		terminating,
		pending,
		unready,
		testPod("ready-runner", "node-1", nil),
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	if resp.Pod.Name != "ready-runner" {
		t.Fatalf("claimed pod = %q, want ready-runner", resp.Pod.Name)
	}
}

func TestAllocUsesPodScopedServerWireGuardPublicKey(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
		testPod("runner-2", "node-1", nil),
	)

	first, status, err := op.Alloc(t.Context(), "alloc-key-1", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("first alloc status=%d err=%v", status, err)
	}

	second, status, err := op.Alloc(t.Context(), "alloc-key-2", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("second alloc status=%d err=%v", status, err)
	}

	if first.Pod.Name == second.Pod.Name {
		t.Fatalf("second alloc reused pod %q", second.Pod.Name)
	}

	if first.WireGuard.ServerPublicKey == second.WireGuard.ServerPublicKey {
		t.Fatal("allocs returned the same server WireGuard public key")
	}
}

func TestDeallocDeletesClaimedRunner(t *testing.T) {
	op := testOperator(
		t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}

	if _, status, err := op.Alloc(t.Context(), "alloc-key", req); err != nil || status != 200 {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	status, err := op.Dealloc(t.Context(), "alloc-key")
	if err != nil {
		t.Fatalf("dealloc: %v", err)
	}

	if status != 204 {
		t.Fatalf("dealloc status = %d, want 204", status)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err == nil {
		t.Fatal("allocated pod still exists after dealloc")
	}
}

func TestDeallocIsIdempotentWhenClaimIsMissing(t *testing.T) {
	op := testOperator(t)

	status, err := op.Dealloc(t.Context(), "alloc-key")
	if err != nil {
		t.Fatalf("dealloc: %v", err)
	}

	if status != 204 {
		t.Fatalf("dealloc status = %d, want 204", status)
	}
}

func TestAggregatedDiscoveryRequiresTrustedFrontProxy(t *testing.T) {
	op := testAggregatedOperator(t, true)

	groupReq := trustedAggregatedRequest(t, op, http.MethodGet, aggregatedAPIGroupPath, nil)
	groupRecorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(groupRecorder, groupReq)

	if groupRecorder.Code != http.StatusOK {
		t.Fatalf("group discovery status = %d, want %d", groupRecorder.Code, http.StatusOK)
	}

	if !bytes.Contains(groupRecorder.Body.Bytes(), []byte(apiGroupVersion)) {
		t.Fatalf("group discovery body = %s", groupRecorder.Body.String())
	}

	versionReq := trustedAggregatedRequest(t, op, http.MethodGet, aggregatedAPIVersionPath, nil)
	versionRecorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(versionRecorder, versionReq)

	if versionRecorder.Code != http.StatusOK {
		t.Fatalf("version discovery status = %d, want %d", versionRecorder.Code, http.StatusOK)
	}

	if !bytes.Contains(versionRecorder.Body.Bytes(), []byte(`"allocs"`)) || !bytes.Contains(versionRecorder.Body.Bytes(), []byte(`"deallocs"`)) {
		t.Fatalf("version discovery body = %s", versionRecorder.Body.String())
	}

	untrustedReq := httptest.NewRequest(http.MethodGet, aggregatedAPIVersionPath, nil)
	untrustedRecorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(untrustedRecorder, untrustedReq)

	if untrustedRecorder.Code != http.StatusForbidden {
		t.Fatalf("untrusted discovery status = %d, want %d", untrustedRecorder.Code, http.StatusForbidden)
	}
}

func TestAllocEndpointAuthorizesSharedAllocAction(t *testing.T) {
	op := testAggregatedOperator(
		t, true,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	reqBody, err := json.Marshal(AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatal(err)
	}

	req := trustedAggregatedRequest(t, op, http.MethodPost, allocsPath, bytes.NewReader(reqBody))
	req.Header.Set(idempotencyKeyHeader, "alloc-key")
	req.Header.Set(remoteUserHeader, "alice")
	req.Header.Add(remoteGroupHeader, "team-a")
	req.Header.Add(remoteGroupHeader, "team-b,team-c")

	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("alloc endpoint status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestAllocEndpointAcceptsBodyIdempotencyKey(t *testing.T) {
	op := testAggregatedOperator(
		t, true,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	reqBody, err := json.Marshal(AllocRequest{IdempotencyKey: "alloc-key", WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatal(err)
	}

	req := trustedAggregatedRequest(t, op, http.MethodPost, allocsPath, bytes.NewReader(reqBody))
	req.Header.Set(remoteUserHeader, "alice")

	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("alloc endpoint status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestAllocEndpointRejectsMissingRemoteUser(t *testing.T) {
	op := testAggregatedOperator(
		t, true,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	reqBody, err := json.Marshal(AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatal(err)
	}

	req := trustedAggregatedRequest(t, op, http.MethodPost, allocsPath, bytes.NewReader(reqBody))
	req.Header.Set(idempotencyKeyHeader, "alloc-key")

	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("alloc endpoint status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestAllocEndpointRejectsDeniedSubjectAccessReview(t *testing.T) {
	op := testAggregatedOperator(
		t, false,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	reqBody, err := json.Marshal(AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatal(err)
	}

	req := trustedAggregatedRequest(t, op, http.MethodPost, allocsPath, bytes.NewReader(reqBody))
	req.Header.Set(idempotencyKeyHeader, "alloc-key")
	req.Header.Set(remoteUserHeader, "alice")

	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("alloc endpoint status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestDeallocEndpointDeletesAllocatedRunner(t *testing.T) {
	op := testAggregatedOperator(
		t, true,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}
	if _, status, err := op.Alloc(t.Context(), "alloc-key", req); err != nil || status != 200 {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	httpReq := trustedAggregatedRequest(t, op, http.MethodPost, deallocsPath, nil)
	httpReq.Header.Set(idempotencyKeyHeader, "alloc-key")
	httpReq.Header.Set(remoteUserHeader, "alice")

	recorder := httptest.NewRecorder()

	op.Handler().ServeHTTP(recorder, httpReq)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("dealloc endpoint status = %d, want %d", recorder.Code, http.StatusNoContent)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err == nil {
		t.Fatal("claimed pod still exists after dealloc endpoint call")
	}
}

func TestDeallocEndpointAcceptsBodyIdempotencyKey(t *testing.T) {
	op := testAggregatedOperator(
		t, true,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)

	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}
	if _, status, err := op.Alloc(t.Context(), "alloc-key", req); err != nil || status != 200 {
		t.Fatalf("alloc status=%d err=%v", status, err)
	}

	reqBody, err := json.Marshal(DeallocRequest{IdempotencyKey: "alloc-key"})
	if err != nil {
		t.Fatal(err)
	}

	httpReq := trustedAggregatedRequest(t, op, http.MethodPost, deallocsPath, bytes.NewReader(reqBody))
	httpReq.Header.Set(remoteUserHeader, "alice")

	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, httpReq)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("dealloc endpoint status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestReconcileDeletesExpiredClaimedRunner(t *testing.T) {
	oldClaim := map[string]string{
		AnnotationIdempotencyKeyHash: hashString("old-claim"),
		AnnotationRequestHash:        hashString("old-request"),
		AnnotationClaimedAt:          time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}
	freshClaim := map[string]string{
		AnnotationIdempotencyKeyHash: hashString("fresh-claim"),
		AnnotationRequestHash:        hashString("fresh-request"),
		AnnotationClaimedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	op := testOperator(
		t,
		testPod("old-runner", "node-1", oldClaim),
		testPod("fresh-runner", "node-1", freshClaim),
	)
	op.Config.PlaypenTTL = time.Hour

	if err := op.ReconcileRunners(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "old-runner"}, pod); err == nil {
		t.Fatal("expired pod still exists after reconcile")
	}

	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "fresh-runner"}, pod); err != nil {
		t.Fatalf("fresh pod was deleted: %v", err)
	}
}

func TestReconcileDeletesClaimWithInvalidClaimedAt(t *testing.T) {
	claim := map[string]string{
		AnnotationIdempotencyKeyHash: hashString("claim"),
		AnnotationRequestHash:        hashString("request"),
		AnnotationClaimedAt:          "not-a-time",
	}
	op := testOperator(t, testPod("runner-1", "node-1", claim))
	op.Config.PlaypenTTL = time.Hour

	if err := op.ReconcileRunners(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err == nil {
		t.Fatal("pod with invalid claimed-at still exists after reconcile")
	}
}

func TestReconcileCreatesIdleRunnerPodsWithUniqueHostPorts(t *testing.T) {
	op := testOperator(t)
	op.Config.RunnerAMD64Count = 2
	op.Config.RunnerARM64Count = 1
	op.Config.RunnerWireGuardHostPortStart = 52000
	op.Config.RunnerWireGuardHostPortEnd = 52002

	if err := op.ReconcileRunners(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pods := &corev1.PodList{}
	if err := op.Client.List(t.Context(), pods, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/name": "playpen-runner"}); err != nil {
		t.Fatal(err)
	}

	if len(pods.Items) != 3 {
		t.Fatalf("pods = %d, want 3", len(pods.Items))
	}

	hostPorts := map[int32]struct{}{}
	counts := map[string]int{}

	for i := range pods.Items {
		pod := &pods.Items[i]

		hostPort := podWireGuardHostPort(pod)
		if hostPort < 52000 || hostPort > 52002 {
			t.Fatalf("pod %s hostPort = %d, want in range", pod.Name, hostPort)
		}

		if _, ok := hostPorts[hostPort]; ok {
			t.Fatalf("duplicate hostPort %d", hostPort)
		}

		hostPorts[hostPort] = struct{}{}
		counts[podArchitecture(pod)]++

		if pod.Spec.ServiceAccountName != "playpen-runner" {
			t.Fatalf("pod %s serviceAccount = %q", pod.Name, pod.Spec.ServiceAccountName)
		}

		if pod.Spec.NodeSelector["kubernetes.io/arch"] != podArchitecture(pod) {
			t.Fatalf("pod %s node selector = %#v", pod.Name, pod.Spec.NodeSelector)
		}
	}

	if counts[ArchitectureAMD64] != 2 || counts[ArchitectureARM64] != 1 {
		t.Fatalf("counts = %#v, want amd64=2 arm64=1", counts)
	}
}

func TestReconcileSkipsUsedRunnerHostPorts(t *testing.T) {
	existing := testPodWithHostPort("existing-runner", "node-1", 52000)
	op := testOperator(t, existing)
	op.Config.RunnerAMD64Count = 2
	op.Config.RunnerARM64Count = 0
	op.Config.RunnerWireGuardHostPortStart = 52000
	op.Config.RunnerWireGuardHostPortEnd = 52001

	if err := op.ReconcileRunners(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pods := &corev1.PodList{}
	if err := op.Client.List(t.Context(), pods, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/name": "playpen-runner"}); err != nil {
		t.Fatal(err)
	}

	if len(pods.Items) != 2 {
		t.Fatalf("pods = %d, want 2", len(pods.Items))
	}

	seen := map[int32]struct{}{}
	for i := range pods.Items {
		seen[podWireGuardHostPort(&pods.Items[i])] = struct{}{}
	}

	if _, ok := seen[52000]; !ok {
		t.Fatal("existing hostPort 52000 was not preserved")
	}

	if _, ok := seen[52001]; !ok {
		t.Fatal("new runner did not use next free hostPort 52001")
	}
}

func TestReconcileReportsNoFreeRunnerHostPorts(t *testing.T) {
	op := testOperator(t, testPodWithHostPort("existing-runner", "node-1", 52000))
	op.Config.RunnerAMD64Count = 2
	op.Config.RunnerARM64Count = 0
	op.Config.RunnerWireGuardHostPortStart = 52000
	op.Config.RunnerWireGuardHostPortEnd = 52000

	if err := op.ReconcileRunners(t.Context()); err == nil {
		t.Fatal("reconcile succeeded without a free hostPort")
	}
}

func TestReconcileCreatesIdleControlPlanePodsPerVersion(t *testing.T) {
	op := testOperator(t)
	op.Config.RunnerAMD64Count = 0
	op.Config.RunnerARM64Count = 0
	op.Config.ControlPlaneCount = 1
	op.Config.ControlPlaneVersions = []string{"v1.33.0", "1.34.0"}
	op.Config.ControlPlaneAPIServerHostPortStart = 16443
	op.Config.ControlPlaneAPIServerHostPortEnd = 16444

	if err := op.ReconcileRunners(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pods := &corev1.PodList{}
	if err := op.Client.List(t.Context(), pods, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/name": controlPlaneAppName}); err != nil {
		t.Fatal(err)
	}

	if len(pods.Items) != 2 {
		t.Fatalf("pods = %d, want 2", len(pods.Items))
	}

	versions := map[string]int{}
	hostPorts := map[int32]struct{}{}

	for i := range pods.Items {
		pod := &pods.Items[i]
		versions[podKubernetesVersion(pod)]++

		hostPort := podControlPlaneHostPort(pod)
		if hostPort < 16443 || hostPort > 16444 {
			t.Fatalf("pod %s hostPort = %d", pod.Name, hostPort)
		}

		if _, ok := hostPorts[hostPort]; ok {
			t.Fatalf("duplicate hostPort %d", hostPort)
		}

		hostPorts[hostPort] = struct{}{}

		if pod.Spec.ServiceAccountName != "playpen-control-plane" {
			t.Fatalf("pod %s serviceAccount = %q", pod.Name, pod.Spec.ServiceAccountName)
		}

		controlPlaneContainer := pod.Spec.Containers[0]
		if !containsString(controlPlaneContainer.Args, "--write-kubeconfig="+controlPlaneKubeconfig) {
			t.Fatalf("pod %s k3s args = %#v, want explicit shared kubeconfig path", pod.Name, controlPlaneContainer.Args)
		}

		helperContainer := pod.Spec.Containers[1]
		if len(helperContainer.Command) != 1 || helperContainer.Command[0] != "/unbounded/bin/playpen-runner" {
			t.Fatalf("pod %s helper command = %#v, want playpen-runner", pod.Name, helperContainer.Command)
		}

		if len(helperContainer.Args) == 0 || helperContainer.Args[0] != "control-plane" {
			t.Fatalf("pod %s helper args = %#v, want control-plane subcommand", pod.Name, helperContainer.Args)
		}
	}

	if versions["v1.33.0"] != 1 || versions["v1.34.0"] != 1 {
		t.Fatalf("versions = %#v", versions)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func TestManagedPodsInheritKubernetesServiceEnvOverrides(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "172.18.0.3")
	t.Setenv("KUBERNETES_SERVICE_PORT", "6443")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "6443")

	op := testOperator(t)

	runnerPod := op.runnerPod(ArchitectureAMD64, 51820)
	assertEnvVar(t, runnerPod.Spec.Containers[0].Env, "KUBERNETES_SERVICE_HOST", "172.18.0.3")
	assertEnvVar(t, runnerPod.Spec.Containers[0].Env, "KUBERNETES_SERVICE_PORT", "6443")
	assertEnvVar(t, runnerPod.Spec.Containers[0].Env, "KUBERNETES_SERVICE_PORT_HTTPS", "6443")

	cpPod := op.controlPlanePod("v1.33.0", 16443)
	assertEnvVar(t, cpPod.Spec.Containers[1].Env, "KUBERNETES_SERVICE_HOST", "172.18.0.3")
	assertEnvVar(t, cpPod.Spec.Containers[1].Env, "KUBERNETES_SERVICE_PORT", "6443")
	assertEnvVar(t, cpPod.Spec.Containers[1].Env, "KUBERNETES_SERVICE_PORT_HTTPS", "6443")
}

func assertEnvVar(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()

	for _, entry := range env {
		if entry.Name == name {
			if entry.Value != want {
				t.Fatalf("env %s = %q, want %q", name, entry.Value, want)
			}

			return
		}
	}

	t.Fatalf("env %s missing in %#v", name, env)
}

func testOperator(t *testing.T, objects ...client.Object) *Operator {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	op := &Operator{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Config: DefaultConfig(),
		Scheme: scheme,
	}

	return op
}

var testAggregatedClientCerts = map[*Operator]*x509.Certificate{}

func testAggregatedOperator(t *testing.T, sarAllowed bool, objects ...client.Object) *Operator {
	t.Helper()

	op := testOperator(t, objects...)
	leaf, pool := testFrontProxyCert(t, "front-proxy-client")
	op.aggregatedClientCAs = pool
	op.aggregatedClientAllowedCNs = map[string]struct{}{leaf.Subject.CommonName: {}}
	testAggregatedClientCerts[op] = leaf
	op.KubeClient = kubefake.NewClientset()
	op.KubeClient.(*kubefake.Clientset).PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)

		sar := createAction.GetObject().(*authzv1.SubjectAccessReview)
		if sar.Spec.User != "alice" {
			t.Fatalf("SAR user = %q, want alice", sar.Spec.User)
		}

		if sar.Spec.ResourceAttributes == nil {
			t.Fatal("SAR missing resource attributes")
		}

		attrs := sar.Spec.ResourceAttributes
		if attrs.Verb != "create" || attrs.Group != apiGroup || (attrs.Resource != "allocs" && attrs.Resource != "deallocs") {
			t.Fatalf("SAR attrs = %#v, want create %s allocs or deallocs", attrs, apiGroup)
		}

		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: sarAllowed}}, nil
	})

	return op
}

func trustedAggregatedRequest(t *testing.T, op *Operator, method, path string, body *bytes.Reader) *http.Request {
	t.Helper()

	if body == nil {
		body = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, body)

	_, ok := op.aggregatedClientAllowedCNs["front-proxy-client"]
	if !ok {
		t.Fatal("test operator is missing front-proxy-client allowed CN")
	}

	cert := testAggregatedClientCerts[op]
	if cert == nil {
		t.Fatal("test operator is missing client certificate")
	}

	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}

	return req
}

func testFrontProxyCert(t *testing.T, commonName string) (*x509.Certificate, *x509.CertPool) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test-front-proxy-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	clientCert, err := x509.ParseCertificate(clientDER)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return clientCert, pool
}

func testPod(name, nodeName string, annotations map[string]string) *corev1.Pod {
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[AnnotationServerWireGuardPublicKey] = podServerPublicKey(name)

	pod := testPodWithAnnotations(name, nodeName, annotations)
	setPodWireGuardHostPort(pod, testHostPort(name))

	return pod
}

func testPodWithArchitecture(name, nodeName, architecture string) *corev1.Pod {
	pod := testPod(name, nodeName, nil)
	pod.Labels[LabelArchitecture] = architecture

	return pod
}

func testPodWithHostPort(name, nodeName string, hostPort int32) *corev1.Pod {
	pod := testPod(name, nodeName, nil)
	setPodWireGuardHostPort(pod, hostPort)

	return pod
}

func testControlPlanePod(name, nodeName, kubernetesVersion string, hostPort int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "playpen",
			Labels: map[string]string{
				"app.kubernetes.io/name":      controlPlaneAppName,
				"app.kubernetes.io/component": controlPlaneComponent,
				LabelResourceType:             ResourceTypeControlPlane,
				LabelKubernetesVersion:        kubernetesVersion,
			},
			Annotations: map[string]string{
				AnnotationControlPlaneKubeconfig:  testKubeconfig("https://127.0.0.1:6443"),
				AnnotationControlPlaneGuestServer: "https://192.168.200.1:6443",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: controlPlaneContainerName,
					Ports: []corev1.ContainerPort{
						{Name: controlPlaneAPIPort, Protocol: corev1.ProtocolTCP, ContainerPort: 6443, HostPort: hostPort},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  controlPlaneContainerName,
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				},
				{
					Name:  controlPlaneHelperName,
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				},
			},
		},
	}
}

func testKubeconfig(server string) string {
	return `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: ` + server + `
    insecure-skip-tls-verify: true
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test-token
`
}

func setPodWireGuardHostPort(pod *corev1.Pod, hostPort int32) {
	pod.Spec.Containers = []corev1.Container{
		{
			Name: "runner",
			Ports: []corev1.ContainerPort{
				{
					Name:          "wireguard",
					Protocol:      corev1.ProtocolUDP,
					ContainerPort: 51820,
					HostPort:      hostPort,
				},
			},
		},
	}
}

func testHostPort(name string) int32 {
	var sum int32
	for _, r := range name {
		sum += r
	}

	return 51000 + sum%1000
}

func testPodWithAnnotations(name, nodeName string, annotations map[string]string) *corev1.Pod {
	if annotations == nil {
		annotations = map[string]string{}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "playpen",
			Labels:      map[string]string{"app.kubernetes.io/name": "playpen-runner"},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "runner"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "runner",
					Ready: true,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
					},
				},
			},
		},
	}
}

func podServerPublicKey(name string) string {
	key := wgtypes.Key{}
	for i := range key {
		key[i] = byte(name[i%len(name)]) + byte(i)
	}

	return key.String()
}

func testNode(name, externalIP string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if externalIP != "" {
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: externalIP})
	}

	return node
}

func testPublicKey(t *testing.T) string {
	t.Helper()

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	return key.PublicKey().String()
}
