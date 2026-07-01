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
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestAllocAllocatesRunnerAndCreatesClusterNodePortService(t *testing.T) {
	op := testOperator(t,
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

	if resp.Endpoint.ExternalTrafficPolicy != string(corev1.ServiceExternalTrafficPolicyCluster) {
		t.Fatalf("externalTrafficPolicy = %q, want Cluster", resp.Endpoint.ExternalTrafficPolicy)
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

	services := &corev1.ServiceList{}
	if err := op.Client.List(t.Context(), services, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/component": "runner-nodeport"}); err != nil {
		t.Fatal(err)
	}

	if len(services.Items) != 1 {
		t.Fatalf("services = %d, want 1", len(services.Items))
	}

	service := services.Items[0]
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("service type = %s, want NodePort", service.Spec.Type)
	}

	if service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Fatalf("service externalTrafficPolicy = %s, want Cluster", service.Spec.ExternalTrafficPolicy)
	}

	if service.Spec.Selector[LabelAllocated] != pod.Labels[LabelAllocated] {
		t.Fatalf("service selector does not match allocated pod")
	}

	if len(service.Spec.Ports) != 1 {
		t.Fatalf("service ports = %d, want only the WireGuard port", len(service.Spec.Ports))
	}

	if service.Spec.Ports[0].Name != "wireguard" || service.Spec.Ports[0].Protocol != corev1.ProtocolUDP {
		t.Fatalf("service port = %#v, want only WireGuard UDP", service.Spec.Ports[0])
	}
}

func TestAllocIsIdempotentAndConflictsOnDifferentRequest(t *testing.T) {
	op := testOperator(t,
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
	op := testOperator(t,
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
	op := testOperator(t,
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
	op := testOperator(t,
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
	op := testOperator(t,
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

func TestPatchClaimAllowsSameRequestAfterClaim(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := AllocRequest{WireGuardPublicKey: testPublicKey(t)}
	keyHash := hashString("alloc-key")
	reqHash := hashString(req.WireGuardPublicKey)

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := op.patchClaim(t.Context(), pod, keyHash, reqHash, req.WireGuardPublicKey)
	if err != nil || !ok {
		t.Fatalf("first patch claimed=%v err=%v", ok, err)
	}

	claimed, ok, err = op.patchClaim(t.Context(), claimed, keyHash, reqHash, req.WireGuardPublicKey)
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}

	if ok {
		t.Fatal("second patch reported a new claim")
	}

	if claimed.Name != "runner-1" {
		t.Fatalf("claimed pod = %q", claimed.Name)
	}

	_, _, err = op.patchClaim(t.Context(), claimed, keyHash, hashString(testPublicKey(t)), req.WireGuardPublicKey)
	if err != errIdempotencyRequestConflict {
		t.Fatalf("different request err = %v, want idempotency conflict", err)
	}
}

func TestAllocUsesOtherNodeExternalIPWhenRunnerNodeIsPrivate(t *testing.T) {
	op := testOperator(t,
		testNode("private-node", ""),
		testNode("gateway-node", "20.30.40.50"),
		testPod("runner-1", "private-node", nil),
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.Endpoint.Host != "20.30.40.50" {
		t.Fatalf("endpoint host = %q, want gateway node external ip", resp.Endpoint.Host)
	}

	if resp.Pod.NodePublicIP != "" {
		t.Fatalf("pod node public ip = %q, want empty", resp.Pod.NodePublicIP)
	}
}

func TestAllocRequiresAnyNodeExternalIP(t *testing.T) {
	op := testOperator(t,
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
	op := testOperator(t,
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

	op := testOperator(t,
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
	op := testOperator(t,
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

func TestAllocPatchesExistingNodePortServiceToClusterPolicy(t *testing.T) {
	pod := testPod("runner-1", "node-1", nil)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceNameForPod(pod),
			Namespace: "playpen",
			Labels: map[string]string{
				"app.kubernetes.io/component":    "runner-nodeport",
				"playpen.unbounded-cloud.io/pod": pod.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeNodePort,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector:              map[string]string{LabelAllocated: allocationIDForPod(pod)},
			Ports: []corev1.ServicePort{
				{
					Name:       "wireguard",
					Protocol:   corev1.ProtocolUDP,
					Port:       51820,
					TargetPort: intstr.FromInt(51820),
					NodePort:   32000,
				},
			},
		},
	}
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		pod,
		service,
	)

	resp, status, err := op.Alloc(t.Context(), "alloc-key", AllocRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.Endpoint.ExternalTrafficPolicy != string(corev1.ServiceExternalTrafficPolicyCluster) {
		t.Fatalf("externalTrafficPolicy = %q, want Cluster", resp.Endpoint.ExternalTrafficPolicy)
	}

	if resp.Endpoint.WireGuardUDPPort != 32000 {
		t.Fatalf("wireguard node port = %d, want 32000", resp.Endpoint.WireGuardUDPPort)
	}

	patched := &corev1.Service{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: service.Name}, patched); err != nil {
		t.Fatal(err)
	}

	if patched.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Fatalf("service externalTrafficPolicy = %s, want Cluster", patched.Spec.ExternalTrafficPolicy)
	}

	if patched.Spec.Ports[0].NodePort != 32000 {
		t.Fatalf("service nodePort = %d, want 32000", patched.Spec.Ports[0].NodePort)
	}
}

func TestDeallocDeletesClaimedRunnerAndService(t *testing.T) {
	op := testOperator(t,
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

	services := &corev1.ServiceList{}
	if err := op.Client.List(t.Context(), services, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/component": "runner-nodeport"}); err != nil {
		t.Fatal(err)
	}

	if len(services.Items) != 0 {
		t.Fatalf("services = %d, want 0", len(services.Items))
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

func TestLegacyStandaloneEndpointsAreRemoved(t *testing.T) {
	op := testOperator(t)
	recorder := httptest.NewRecorder()
	op.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/playpen/v1/claims", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("legacy claims status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestAllocEndpointAuthorizesSharedAllocAction(t *testing.T) {
	op := testAggregatedOperator(t, true,
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

func TestAllocEndpointRejectsMissingRemoteUser(t *testing.T) {
	op := testAggregatedOperator(t, true,
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
	op := testAggregatedOperator(t, false,
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
	op := testAggregatedOperator(t, true,
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
	op := testOperator(t,
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
		if attrs.Verb != "create" || attrs.Group != apiGroup || attrs.Resource != "allocs" {
			t.Fatalf("SAR attrs = %#v, want create %s allocs", attrs, apiGroup)
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

	return testPodWithAnnotations(name, nodeName, annotations)
}

func testPodWithArchitecture(name, nodeName, architecture string) *corev1.Pod {
	pod := testPod(name, nodeName, nil)
	pod.Labels[LabelArchitecture] = architecture

	return pod
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
		Spec: corev1.PodSpec{NodeName: nodeName},
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
