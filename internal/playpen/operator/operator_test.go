// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestClaimAllocatesRunnerAndCreatesClusterNodePortService(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := ClaimRequest{WireGuardPublicKey: testPublicKey(t)}

	resp, status, err := op.Claim(t.Context(), "claim-key", req)
	if err != nil {
		t.Fatalf("claim: %v", err)
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
	if resp.Endpoint.ExternalTrafficPolicy != string(corev1.ServiceExternalTrafficPolicyCluster) {
		t.Fatalf("externalTrafficPolicy = %q, want Cluster", resp.Endpoint.ExternalTrafficPolicy)
	}
	if resp.WireGuard.ServerPublicKey == "" {
		t.Fatal("server WireGuard public key is empty")
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[AnnotationClientWireGuardPublicKey] != req.WireGuardPublicKey {
		t.Fatalf("pod claim annotation = %q, want %q", pod.Annotations[AnnotationClientWireGuardPublicKey], req.WireGuardPublicKey)
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
}

func TestClaimIsIdempotentAndConflictsOnDifferentRequest(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := ClaimRequest{WireGuardPublicKey: testPublicKey(t)}

	first, status, err := op.Claim(t.Context(), "claim-key", req)
	if err != nil || status != 200 {
		t.Fatalf("first claim status=%d err=%v", status, err)
	}
	second, status, err := op.Claim(t.Context(), "claim-key", req)
	if err != nil || status != 200 {
		t.Fatalf("second claim status=%d err=%v", status, err)
	}
	if second.Pod.Name != first.Pod.Name {
		t.Fatalf("second pod = %q, want %q", second.Pod.Name, first.Pod.Name)
	}

	_, status, err = op.Claim(t.Context(), "claim-key", ClaimRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("different request with same idempotency key succeeded")
	}
	if status != 409 {
		t.Fatalf("conflict status = %d, want 409", status)
	}
}

func TestClaimUsesOtherNodeExternalIPWhenRunnerNodeIsPrivate(t *testing.T) {
	op := testOperator(t,
		testNode("private-node", ""),
		testNode("gateway-node", "20.30.40.50"),
		testPod("runner-1", "private-node", nil),
	)

	resp, status, err := op.Claim(t.Context(), "claim-key", ClaimRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatalf("claim: %v", err)
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

func TestClaimRequiresAnyNodeExternalIP(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", ""),
		testPod("runner-1", "node-1", nil),
	)

	_, status, err := op.Claim(t.Context(), "claim-key", ClaimRequest{WireGuardPublicKey: testPublicKey(t)})
	if err == nil {
		t.Fatal("claim succeeded without any node ExternalIP")
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

func TestClaimPatchesExistingNodePortServiceToClusterPolicy(t *testing.T) {
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

	resp, status, err := op.Claim(t.Context(), "claim-key", ClaimRequest{WireGuardPublicKey: testPublicKey(t)})
	if err != nil {
		t.Fatalf("claim: %v", err)
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

func TestReleaseDeletesClaimedRunnerAndService(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := ClaimRequest{WireGuardPublicKey: testPublicKey(t)}

	if _, status, err := op.Claim(t.Context(), "claim-key", req); err != nil || status != 200 {
		t.Fatalf("claim status=%d err=%v", status, err)
	}
	status, err := op.Release(t.Context(), "claim-key")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if status != 204 {
		t.Fatalf("release status = %d, want 204", status)
	}

	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err == nil {
		t.Fatal("claimed pod still exists after release")
	}

	services := &corev1.ServiceList{}
	if err := op.Client.List(t.Context(), services, client.InNamespace("playpen"), client.MatchingLabels{"app.kubernetes.io/component": "runner-nodeport"}); err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 0 {
		t.Fatalf("services = %d, want 0", len(services.Items))
	}
}

func TestReleaseIsIdempotentWhenClaimIsMissing(t *testing.T) {
	op := testOperator(t)

	status, err := op.Release(t.Context(), "claim-key")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if status != 204 {
		t.Fatalf("release status = %d, want 204", status)
	}
}

func TestReleaseEndpointDeletesClaimedRunner(t *testing.T) {
	op := testOperator(t,
		testNode("node-1", "20.30.40.50"),
		testPod("runner-1", "node-1", nil),
	)
	req := ClaimRequest{WireGuardPublicKey: testPublicKey(t)}
	if _, status, err := op.Claim(t.Context(), "claim-key", req); err != nil || status != 200 {
		t.Fatalf("claim status=%d err=%v", status, err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/playpen/v1/releases", nil)
	httpReq.Header.Set(idempotencyKeyHeader, "claim-key")
	recorder := httptest.NewRecorder()

	op.Handler().ServeHTTP(recorder, httpReq)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("release endpoint status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	pod := &corev1.Pod{}
	if err := op.Client.Get(t.Context(), client.ObjectKey{Namespace: "playpen", Name: "runner-1"}, pod); err == nil {
		t.Fatal("claimed pod still exists after release endpoint call")
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
	if err := op.EnsureRunnerWireGuardSecret(context.Background()); err != nil {
		t.Fatal(err)
	}

	return op
}

func testPod(name, nodeName string, annotations map[string]string) *corev1.Pod {
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
	}
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
