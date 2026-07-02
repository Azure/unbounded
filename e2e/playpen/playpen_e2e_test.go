//go:build e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	playpenclient "github.com/Azure/unbounded/internal/playpen/client"
	"github.com/Azure/unbounded/internal/playpen/operator"
	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	clusterName           = "playpen-e2e"
	imageTag              = "playpen:e2e"
	netControllerImageTag = "unbounded-net-controller:e2e"
	netNodeImageTag       = "unbounded-net-node:e2e"
	namespace             = "playpen"
	netNamespace          = "unbounded-net"
	runnerSite            = "playpen-runner"
	externalSite          = "playpen-external"
	externalPodCIDRPool   = "10.250.0.0/27"
)

type harness struct {
	t           *testing.T
	repoRoot    string
	artifacts   string
	keepCluster bool

	restConfig     *rest.Config
	kubeconfigPath string
	kubeClient     kubernetes.Interface
}

type operatorTransport struct {
	op *operator.Operator
}

func (t operatorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/apis/playpen.unbounded-cloud.io/v1alpha1/allocs":
		return t.allocate(req)
	case "/apis/playpen.unbounded-cloud.io/v1alpha1/deallocs":
		return t.deallocate(req)
	default:
		return jsonResponse(req, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (t operatorTransport) allocate(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(req, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}

	var allocReq operator.AllocRequest
	if err := json.NewDecoder(req.Body).Decode(&allocReq); err != nil {
		return jsonResponse(req, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
	}

	allocResp, status, err := t.op.Alloc(req.Context(), req.Header.Get("Idempotency-Key"), allocReq)
	if err != nil {
		return jsonResponse(req, status, map[string]string{"error": err.Error()})
	}
	return jsonResponse(req, status, allocResp)
}

func (t operatorTransport) deallocate(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(req, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}

	status, err := t.op.Dealloc(req.Context(), req.Header.Get("Idempotency-Key"))
	if err != nil {
		return jsonResponse(req, status, map[string]string{"error": err.Error()})
	}

	return jsonResponse(req, status, nil)
}

func jsonResponse(req *http.Request, status int, value any) (*http.Response, error) {
	var body []byte

	if value != nil {
		var err error

		body, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}

	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func TestPlaypenBasicLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	h := newHarness(t)
	h.checkPrereqs()
	h.bootCluster(ctx)

	if !h.keepCluster {
		t.Cleanup(func() { h.teardown(context.Background()) })
	}

	h.buildAndLoadImage(ctx)
	h.buildAndLoadNetImages(ctx)
	h.deployUnboundedNet(ctx)
	h.configureUnboundedNetTopology(ctx)
	externalClientInternalIP := h.externalClientInternalIP(ctx)
	h.renderManifests(ctx)
	h.applyManifests(ctx)
	h.patchNodeExternalIPs(ctx)
	h.waitForRollouts(ctx)
	h.patchNodeExternalIPs(ctx)
	h.waitForReadyRunner(ctx)

	client, err := h.newPlaypenClient()
	if err != nil {
		t.Fatalf("create playpen client: %v", err)
	}

	allocated, err := client.Allocate(ctx, playpenclient.AllocateOptions{
		Architecture:             runner.ArchitectureAMD64,
		ExternalClientInternalIP: externalClientInternalIP,
	})
	if err != nil {
		t.Fatalf("allocate playpen: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()

		if err := allocated.Close(closeCtx); err != nil {
			t.Logf("close playpen during cleanup: %v", err)
		}
	})

	if allocated.Metadata.Pod.Namespace != namespace || allocated.Metadata.Pod.Name == "" {
		t.Fatalf("allocation returned unexpected pod metadata: %#v", allocated.Metadata.Pod)
	}

	if allocated.Metadata.Endpoint.Host == "" {
		t.Fatalf("allocation returned incomplete endpoint metadata: %#v", allocated.Metadata.Endpoint)
	}

	if allocated.Metadata.ExternalClient.NodeName == "" || allocated.Metadata.ExternalClient.Site == "" || allocated.Metadata.ExternalClient.PodCIDR == "" {
		t.Fatalf("allocation returned incomplete external client metadata: %#v", allocated.Metadata.ExternalClient)
	}

	if allocated.Metadata.VXLAN.VNI == 0 || allocated.Metadata.VXLAN.UDPPort == 0 {
		t.Fatalf("allocation returned incomplete vxlan metadata: %#v", allocated.Metadata.VXLAN)
	}

	h.waitForPodAnnotation(ctx, allocated.Metadata.Pod.Name, operator.AnnotationIdempotencyKeyHash, true)
	externalNodeCancel := h.startExternalUnboundedNetNode(ctx, allocated.Metadata.ExternalClient.NodeName)
	t.Cleanup(externalNodeCancel)
	h.waitForExternalUnboundedNet(ctx, allocated.Metadata.VXLAN.ClientAddress, allocated.Metadata.VXLAN.ServerAddress)

	if err := allocated.ConfigureTunnel(ctx); err != nil {
		t.Fatalf("configure tunnel: %v", err)
	}

	h.verifyTunnelInterfaces(ctx, allocated)
	h.verifyTunnelConnectivity(ctx, allocated)

	if err := allocated.Close(ctx); err != nil {
		t.Fatalf("close playpen: %v", err)
	}

	h.waitForPodDeleted(ctx, allocated.Metadata.Pod.Name)
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	repoRoot := filepath.Dir(filepath.Dir(wd))

	artifacts := filepath.Join(wd, ".artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	return &harness{t: t, repoRoot: repoRoot, artifacts: artifacts, keepCluster: os.Getenv("E2E_KEEP") == "1"}
}

func (h *harness) checkPrereqs() {
	h.t.Helper()

	for _, bin := range []string{"bridge", "docker", "ip", "iptables", "kind", "kubectl", "make", "sysctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}

	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("sudo"); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", "sudo")
		}

		if err := h.run(context.Background(), "sudo", "-n", "true"); err != nil {
			h.t.Skipf("passwordless sudo unavailable (%v); skipping suite", err)
		}
	}

	if err := h.run(context.Background(), "docker", "info"); err != nil {
		h.t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}
}

func (h *harness) bootCluster(ctx context.Context) {
	h.t.Helper()

	if !h.keepCluster && h.clusterExists(ctx) {
		h.teardown(ctx)
	}

	if h.clusterExists(ctx) {
		h.initClients()

		return
	}

	cfg := filepath.Join(h.repoRoot, "e2e", "playpen", "kind-config.yaml")
	if err := h.run(ctx, "kind", "create", "cluster", "--name", clusterName, "--config", cfg, "--wait", "120s"); err != nil {
		h.t.Fatalf("kind create cluster: %v", err)
	}

	h.initClients()
}

func (h *harness) clusterExists(ctx context.Context) bool {
	out, err := h.runOut(ctx, "kind", "get", "clusters")
	if err != nil {
		return false
	}

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true
		}
	}

	return false
}

func (h *harness) initClients() {
	h.t.Helper()

	kubeconfig, err := h.runOut(context.Background(), "kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		h.t.Fatalf("kind get kubeconfig: %v", err)
	}

	kubeconfigPath := filepath.Join(h.artifacts, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		h.t.Fatalf("write kubeconfig: %v", err)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		h.t.Fatalf("parse kubeconfig: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatalf("create kubernetes client: %v", err)
	}

	h.restConfig = restConfig
	h.kubeconfigPath = kubeconfigPath
	h.kubeClient = kubeClient
}

func (h *harness) newPlaypenClient() (*playpenclient.Client, error) {
	h.t.Helper()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	kubeClient, err := ctrlclient.New(h.restConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create operator client: %w", err)
	}

	cfg := operator.DefaultConfig()
	cfg.Namespace = namespace
	cfg.RunnerNamespace = namespace
	cfg.RunnerLabelSelector = "app.kubernetes.io/name=playpen-runner"
	cfg.ExternalClientSite = externalSite

	op := &operator.Operator{
		Client:     kubeClient,
		KubeClient: h.kubeClient,
		Config:     cfg,
		Scheme:     scheme,
	}

	return playpenclient.New(playpenclient.Config{
		RESTConfig: &rest.Config{Host: "http://playpen-e2e.local"},
		HTTPClient: &http.Client{Transport: operatorTransport{op: op}},
	})
}

func (h *harness) buildAndLoadImage(ctx context.Context) {
	h.t.Helper()

	platform := fmt.Sprintf("linux/%s", goArchForDocker(runtime.GOARCH))
	if err := h.run(ctx, "docker", "build", "--platform", platform, "-t", imageTag, "-f", filepath.Join(h.repoRoot, "images", "playpen", "Containerfile"), h.repoRoot); err != nil {
		h.t.Fatalf("build playpen image: %v", err)
	}

	if err := h.run(ctx, "kind", "load", "docker-image", imageTag, "--name", clusterName); err != nil {
		h.t.Fatalf("kind load playpen image: %v", err)
	}
}

func (h *harness) buildAndLoadNetImages(ctx context.Context) {
	h.t.Helper()

	cniArchive := fmt.Sprintf("resources/cni-plugins-linux-%s-v1.9.1.tgz", goArchForDocker(runtime.GOARCH))
	if err := h.run(ctx, "make", cniArchive); err != nil {
		h.t.Fatalf("fetch CNI plugins archive: %v", err)
	}

	for _, build := range []struct {
		target string
		image  string
	}{
		{target: "controller", image: netControllerImageTag},
		{target: "node", image: netNodeImageTag},
	} {
		if err := h.run(
			ctx,
			"docker", "build",
			"--target", build.target,
			"--build-arg", "CNI_PLUGINS_VERSION=v1.9.1",
			"--build-arg", "BUILDARCH="+goArchForDocker(runtime.GOARCH),
			"-t", build.image,
			"-f", filepath.Join(h.repoRoot, "images", "net", "Containerfile"),
			h.repoRoot,
		); err != nil {
			h.t.Fatalf("build unbounded-net %s image: %v", build.target, err)
		}
	}

	for _, image := range []string{netControllerImageTag, netNodeImageTag} {
		if err := h.run(ctx, "kind", "load", "docker-image", image, "--name", clusterName); err != nil {
			h.t.Fatalf("kind load %s: %v", image, err)
		}
	}
}

func (h *harness) deployUnboundedNet(ctx context.Context) {
	h.t.Helper()

	manifestDir := filepath.Join(h.artifacts, "net-rendered")
	if err := os.RemoveAll(manifestDir); err != nil {
		h.t.Fatalf("clean net rendered manifests: %v", err)
	}

	if err := h.run(
		ctx,
		"go", "run", "./hack/cmd/render-manifests",
		"--templates-dir", filepath.Join(h.repoRoot, "deploy", "net"),
		"--output-dir", manifestDir,
		"--set", "Namespace="+netNamespace,
		"--set", "ControllerImage="+netControllerImageTag,
		"--set", "NodeImage="+netNodeImageTag,
		"--set", "ForceNotLeader=false",
		"--set", "AzureTenantID=",
		"--set", "ApiserverURL=",
		"--set", "ControllerManagedKubeProxyEnabled=false",
		"--set", "ControllerRegisterAggregatedAPIServer=false",
		"--set", "NodeStatusWebsocketEnabled=false",
		"--set", "NodeStatusWebsocketApiserverMode=never",
		"--set", "NodeStatusPushEnabled=false",
		"--set", "NodeRemoveConfigurationOnShutdown=true",
	); err != nil {
		h.t.Fatalf("render unbounded-net manifests: %v", err)
	}
	h.patchRenderedNetManifests(ctx, manifestDir)

	if err := h.run(ctx, "kubectl", "apply", "-f", filepath.Join(h.repoRoot, "deploy", "net", "crd")); err != nil {
		h.t.Fatalf("apply unbounded-net CRDs: %v", err)
	}
	if err := h.run(ctx, "kubectl", "apply", "-R", "-f", manifestDir); err != nil {
		h.t.Fatalf("apply unbounded-net manifests: %v", err)
	}
	if err := h.run(ctx, "kubectl", "rollout", "status", "deployment/unbounded-net-controller", "-n", netNamespace, "--timeout=180s"); err != nil {
		h.dumpDiagnostics(context.Background())
		h.t.Fatalf("rollout unbounded-net-controller: %v", err)
	}
}

func (h *harness) patchRenderedNetManifests(ctx context.Context, manifestDir string) {
	h.t.Helper()

	replacements := []struct {
		path string
		old  string
		new  string
	}{
		{path: filepath.Join(manifestDir, "controller", "03-deployment.yaml"), old: "imagePullPolicy: Always", new: "imagePullPolicy: IfNotPresent"},
		{path: filepath.Join(manifestDir, "node", "03-daemonset.yaml"), old: "imagePullPolicy: Always", new: "imagePullPolicy: IfNotPresent"},
	}

	apiServer := "https://" + h.controlPlaneIP(ctx) + ":6443"
	replacements = append(replacements, struct {
		path string
		old  string
		new  string
	}{path: filepath.Join(manifestDir, "01-configmap.yaml"), old: `apiserverURL: ""`, new: `apiserverURL: "` + apiServer + `"`})

	for _, replacement := range replacements {
		data, err := os.ReadFile(replacement.path)
		if err != nil {
			h.t.Fatalf("read %s: %v", replacement.path, err)
		}

		updated := strings.ReplaceAll(string(data), replacement.old, replacement.new)
		if updated == string(data) {
			h.t.Fatalf("%s did not contain %q", replacement.path, replacement.old)
		}

		if err := os.WriteFile(replacement.path, []byte(updated), 0o644); err != nil {
			h.t.Fatalf("write %s: %v", replacement.path, err)
		}
	}
}

func (h *harness) configureUnboundedNetTopology(ctx context.Context) {
	h.t.Helper()

	nodes, err := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if err != nil {
		h.t.Fatalf("list control-plane nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		h.t.Fatal("no control-plane node found")
	}

	runnerNode := nodes.Items[0]
	if err := h.patchNodeSite(ctx, runnerNode.Name, runnerSite); err != nil {
		h.t.Fatalf("patch runner node for unbounded-net: %v", err)
	}

	runnerPodCIDR := runnerNode.Spec.PodCIDR
	if runnerPodCIDR == "" && len(runnerNode.Spec.PodCIDRs) > 0 {
		runnerPodCIDR = runnerNode.Spec.PodCIDRs[0]
	}
	if runnerPodCIDR == "" {
		h.t.Fatalf("runner node %s has no pod CIDR", runnerNode.Name)
	}

	runnerInternalIP := nodeAddress(runnerNode, corev1.NodeInternalIP)
	if runnerInternalIP == "" {
		h.t.Fatalf("runner node %s has no InternalIP", runnerNode.Name)
	}
	externalInternalIP := h.externalClientInternalIP(ctx)

	netYAML := fmt.Sprintf(`apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: %[1]s
spec:
  manageCniPlugin: false
  healthCheckSettings:
    enabled: false
  nodeCidrs:
    - %[2]s/32
  podCidrAssignments:
    - cidrBlocks:
        - %[3]s
      nodeBlockSizes:
        ipv4: 28
  tunnelProtocol: WireGuard
---
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: %[4]s
spec:
  manageCniPlugin: false
  healthCheckSettings:
    enabled: false
  nodeCidrs:
    - %[5]s/32
  podCidrAssignments:
    - cidrBlocks:
        - %[6]s
      nodeBlockSizes:
        ipv4: 28
  tunnelProtocol: WireGuard
---
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: playpen-e2e
spec:
  healthCheckSettings:
    enabled: false
  sites:
    - %[1]s
    - %[4]s
  meshNodes: true
  tunnelProtocol: WireGuard
`, runnerSite, runnerInternalIP, runnerPodCIDR, externalSite, externalInternalIP, externalPodCIDRPool)

	if err := h.runWithInput(ctx, strings.NewReader(netYAML), "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply unbounded-net topology: %v", err)
	}
}

func (h *harness) patchNodeSite(ctx context.Context, nodeName, site string) error {
	patch := map[string]any{
		"metadata": map[string]any{"labels": map[string]string{netv1alpha1.SiteLabelKey: site}},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = h.kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, data, metav1.PatchOptions{})

	return err
}

func (h *harness) renderManifests(ctx context.Context) {
	h.t.Helper()

	manifestDir := filepath.Join(h.artifacts, "rendered")
	if err := os.RemoveAll(manifestDir); err != nil {
		h.t.Fatalf("clean rendered manifests: %v", err)
	}

	if err := h.run(
		ctx,
		"go", "run", "./hack/cmd/render-manifests",
		"--templates-dir", filepath.Join(h.repoRoot, "deploy", "playpen"),
		"--output-dir", manifestDir,
		"--set", "Namespace="+namespace,
		"--set", "PlaypenImage="+imageTag,
		"--set", "RunnerImagePullPolicy=IfNotPresent",
		"--set", "RunnerAMD64Count=1",
		"--set", "RunnerARM64Count=0",
		"--set", "ControlPlaneCount=0",
		"--set", "RunnerRequireKVM=false",
		"--set", "RunnerControlPlaneTolerations=true",
		"--set", "ExternalClientSite="+externalSite,
	); err != nil {
		h.t.Fatalf("render playpen manifests: %v", err)
	}
}

func (h *harness) applyManifests(ctx context.Context) {
	h.t.Helper()

	manifestDir := filepath.Join(h.artifacts, "rendered")
	if err := h.run(ctx, "kubectl", "apply", "-f", manifestDir); err != nil {
		h.t.Fatalf("apply playpen manifests: %v", err)
	}

	operatorPatch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"tolerations": controlPlaneTolerations(),
					"containers": []map[string]any{{
						"name":            "operator",
						"imagePullPolicy": "IfNotPresent",
						"env": []map[string]any{
							{"name": "KUBERNETES_SERVICE_HOST", "value": h.controlPlaneIP(ctx)},
							{"name": "KUBERNETES_SERVICE_PORT", "value": "6443"},
							{"name": "KUBERNETES_SERVICE_PORT_HTTPS", "value": "6443"},
						},
					}},
				},
			},
		},
	}
	h.patchDeployment(ctx, "playpen-operator", operatorPatch)
}

func controlPlaneTolerations() []map[string]any {
	return []map[string]any{
		{
			"key":      "node-role.kubernetes.io/control-plane",
			"operator": "Exists",
			"effect":   "NoSchedule",
		},
		{
			"key":      "node-role.kubernetes.io/master",
			"operator": "Exists",
			"effect":   "NoSchedule",
		},
	}
}

func (h *harness) patchDeployment(ctx context.Context, name string, patch map[string]any) {
	h.t.Helper()

	data, err := json.Marshal(patch)
	if err != nil {
		h.t.Fatalf("marshal deployment patch: %v", err)
	}

	if _, err := h.kubeClient.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, data, metav1.PatchOptions{}); err != nil {
		h.t.Fatalf("patch deployment %s: %v", name, err)
	}
}

func (h *harness) patchNodeExternalIPs(ctx context.Context) {
	h.t.Helper()

	if err := wait(ctx, 30*time.Second, func(ctx context.Context) (bool, error) {
		nodes, err := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		ready := true

		for i := range nodes.Items {
			node := nodes.Items[i]

			internalIP := nodeAddress(node, corev1.NodeInternalIP)
			if internalIP == "" {
				continue
			}

			if nodeAddress(node, corev1.NodeExternalIP) != "" {
				continue
			}

			ready = false
			patch := []map[string]any{{
				"op":    "add",
				"path":  "/status/addresses/-",
				"value": map[string]string{"type": string(corev1.NodeExternalIP), "address": internalIP},
			}}

			data, err := json.Marshal(patch)
			if err != nil {
				return false, err
			}

			if _, err := h.kubeClient.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, data, metav1.PatchOptions{}, "status"); err != nil {
				return false, err
			}
		}

		return ready, nil
	}); err != nil {
		h.t.Fatalf("patch node ExternalIPs: %v", err)
	}
}

func nodeAddress(node corev1.Node, addressType corev1.NodeAddressType) string {
	for _, address := range node.Status.Addresses {
		if address.Type == addressType {
			return address.Address
		}
	}

	return ""
}

func (h *harness) controlPlaneIP(ctx context.Context) string {
	h.t.Helper()

	nodes, err := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if err != nil {
		h.t.Fatalf("list control-plane nodes: %v", err)
	}

	if len(nodes.Items) == 0 {
		h.t.Fatal("no control-plane node found")
	}

	address := nodeAddress(nodes.Items[0], corev1.NodeInternalIP)
	if address == "" {
		h.t.Fatalf("control-plane node %s has no InternalIP", nodes.Items[0].Name)
	}

	return address
}

func (h *harness) externalClientInternalIP(ctx context.Context) string {
	h.t.Helper()

	out, err := h.runOut(ctx, "docker", "network", "inspect", "kind", "--format", "{{range .IPAM.Config}}{{println .Gateway}}{{end}}")
	if err != nil {
		h.t.Fatalf("inspect kind docker network: %v", err)
	}

	for _, field := range strings.Fields(out) {
		address := strings.TrimSpace(field)
		parsed := net.ParseIP(address)
		if parsed != nil && parsed.To4() != nil {
			return address
		}
	}

	h.t.Fatalf("kind docker network has no IPv4 gateway in %q", strings.TrimSpace(out))
	return ""
}

func (h *harness) runnerVXLANAddress(ctx context.Context) string {
	h.t.Helper()

	nodes, err := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if err != nil {
		h.t.Fatalf("list control-plane nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		h.t.Fatal("no control-plane node found")
	}

	podCIDR := nodes.Items[0].Spec.PodCIDR
	if podCIDR == "" && len(nodes.Items[0].Spec.PodCIDRs) > 0 {
		podCIDR = nodes.Items[0].Spec.PodCIDRs[0]
	}
	if podCIDR == "" {
		h.t.Fatalf("control-plane node %s has no pod CIDR", nodes.Items[0].Name)
	}

	return gatewayIPFromCIDR(h.t, podCIDR)
}

func gatewayIPFromCIDR(t *testing.T, cidr string) string {
	t.Helper()

	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("parse CIDR %q: %v", cidr, err)
	}

	addr := prefix.Addr()
	if !addr.Is4() {
		t.Fatalf("CIDR %q is not IPv4", cidr)
	}

	next := addr.Next()
	if !prefix.Contains(next) {
		t.Fatalf("CIDR %q has no usable gateway address", cidr)
	}

	return next.String()
}

func (h *harness) waitForRunnerUnboundedNet(ctx context.Context, runnerVXLANAddress string) {
	h.t.Helper()

	if err := h.run(ctx, "kubectl", "rollout", "status", "daemonset/unbounded-net-node", "-n", netNamespace, "--timeout=180s"); err != nil {
		h.dumpDiagnostics(context.Background())
		h.t.Fatalf("rollout unbounded-net-node: %v", err)
	}

	if err := wait(ctx, 90*time.Second, func(ctx context.Context) (bool, error) {
		out, err := h.runOut(ctx, "docker", "exec", clusterName+"-control-plane", "ip", "addr", "show", "dev", "unbounded0")
		if err != nil {
			return false, nil
		}

		return strings.Contains(out, runnerVXLANAddress), nil
	}); err != nil {
		h.dumpDiagnostics(context.Background())
		h.t.Fatalf("wait for runner unbounded0 address %s: %v", runnerVXLANAddress, err)
	}
}

func (h *harness) startExternalUnboundedNetNode(ctx context.Context, nodeName string) func() {
	h.t.Helper()

	binPath := filepath.Join(h.artifacts, "unbounded-net-node")
	if err := h.run(ctx, "go", "build", "-o", binPath, "./cmd/unbounded-net-node"); err != nil {
		h.t.Fatalf("build external unbounded-net-node: %v", err)
	}

	if err := h.run(ctx, "sudo", "-n", "ip", "link", "delete", "unbounded0"); err != nil {
		h.t.Logf("delete stale host unbounded0: %v", err)
	}

	wgDir := filepath.Join("/var/tmp", "unbounded-playpen-e2e", "external-wg")
	if err := h.run(ctx, "sudo", "-n", "rm", "-rf", wgDir); err != nil {
		h.t.Fatalf("remove stale external wireguard dir: %v", err)
	}
	if err := h.run(ctx, "sudo", "-n", "mkdir", "-p", wgDir); err != nil {
		h.t.Fatalf("mkdir external wireguard dir: %v", err)
	}

	cniConfDir := filepath.Join(h.artifacts, "external-cni")
	configPath := filepath.Join(h.artifacts, "external-unbounded-net-node-config.yaml")
	config := fmt.Sprintf("node:\n  cniConfDir: %q\n", cniConfDir)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		h.t.Fatalf("write external unbounded-net-node config: %v", err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(
		procCtx,
		"sudo", "-n", binPath,
		"--config-file", configPath,
		"--kubeconfig", h.kubeconfigPath,
		"--node-name", nodeName,
		"--wireguard-dir", wgDir,
		"--health-port", "0",
		"--healthcheck-port", "0",
		"--status-push-enabled=false",
		"--status-ws-enabled=false",
		"--status-ws-apiserver-mode=never",
		"--preferred-public-encap=WireGuard",
		"--preferred-private-encap=WireGuard",
		"--remove-configuration-on-shutdown",
	)
	cmd.Dir = h.repoRoot
	logPath := filepath.Join(h.artifacts, "external-unbounded-net-node.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		h.t.Fatalf("create external unbounded-net-node log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close() //nolint:errcheck // Cleanup after failed process start.
		h.t.Fatalf("start external unbounded-net-node: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		logFile.Close() //nolint:errcheck // Test cleanup only.
	}()

	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				h.t.Logf("external unbounded-net-node exited: %v", err)
			}
		case <-time.After(20 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
		if err := h.run(context.Background(), "sudo", "-n", "ip", "link", "delete", "unbounded0"); err != nil {
			h.t.Logf("delete host unbounded0 during cleanup: %v", err)
		}
		if err := h.run(context.Background(), "sudo", "-n", "rm", "-rf", filepath.Dir(wgDir)); err != nil {
			h.t.Logf("remove external wireguard dir during cleanup: %v", err)
		}
	}
}

func (h *harness) waitForExternalUnboundedNet(ctx context.Context, clientVXLANAddress, runnerVXLANAddress string) {
	h.t.Helper()

	if err := wait(ctx, 120*time.Second, func(ctx context.Context) (bool, error) {
		out, err := h.runOut(ctx, "sudo", "-n", "ip", "addr", "show", "dev", "unbounded0")
		if err != nil {
			return false, nil
		}

		return strings.Contains(out, clientVXLANAddress), nil
	}); err != nil {
		h.dumpHostUnboundedNetDiagnostics(context.Background(), runnerVXLANAddress)
		h.dumpDiagnostics(context.Background())
		h.t.Fatalf("wait for external unbounded0 address %s: %v", clientVXLANAddress, err)
	}
}

func (h *harness) waitForRollouts(ctx context.Context) {
	h.t.Helper()

	for _, name := range []string{"playpen-operator"} {
		if err := h.run(ctx, "kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=180s"); err != nil {
			h.dumpDiagnostics(context.Background())
			h.t.Fatalf("rollout %s: %v", name, err)
		}
	}
}

func (h *harness) waitForReadyRunner(ctx context.Context) {
	h.t.Helper()

	if err := wait(ctx, 60*time.Second, func(ctx context.Context) (bool, error) {
		pods, err := h.kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=playpen-runner,playpen.unbounded-cloud.io/architecture=amd64"})
		if err != nil {
			return false, err
		}

		for i := range pods.Items {
			pod := pods.Items[i]
			if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
				continue
			}

			if pod.Annotations[operator.AnnotationIdempotencyKeyHash] != "" {
				continue
			}

			ready := len(pod.Status.ContainerStatuses) > 0
			for _, status := range pod.Status.ContainerStatuses {
				ready = ready && status.Ready && status.State.Running != nil
			}

			if !ready {
				continue
			}

			node, err := h.kubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}

			if nodeAddress(*node, corev1.NodeExternalIP) != "" {
				return true, nil
			}
		}

		return false, nil
	}); err != nil {
		h.dumpDiagnostics(context.Background())
		h.t.Fatalf("wait for ready playpen runner: %v", err)
	}
}

func (h *harness) waitForPodAnnotation(ctx context.Context, podName, annotation string, wantPresent bool) {
	h.t.Helper()

	if err := wait(ctx, 60*time.Second, func(ctx context.Context) (bool, error) {
		pod, err := h.kubeClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}

		_, present := pod.Annotations[annotation]

		return present == wantPresent, nil
	}); err != nil {
		h.t.Fatalf("wait for pod %s annotation %s presence=%t: %v", podName, annotation, wantPresent, err)
	}
}

func (h *harness) waitForPodDeleted(ctx context.Context, podName string) {
	h.t.Helper()

	if err := wait(ctx, 90*time.Second, func(ctx context.Context) (bool, error) {
		_, err := h.kubeClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})

		return err != nil, nil
	}); err != nil {
		h.t.Fatalf("wait for pod %s deletion: %v", podName, err)
	}
}

func (h *harness) verifyTunnelInterfaces(ctx context.Context, allocated *playpenclient.Playpen) {
	h.t.Helper()

	cfg := allocated.TunnelConfig()
	if err := allocated.Run(ctx, "ip", "-d", "link", "show", "dev", cfg.VXLANInterface); err != nil {
		h.t.Fatalf("show vxlan interface %s: %v", cfg.VXLANInterface, err)
	}

	guestGateway := guestGatewayPrefix(h.t, allocated.Metadata.Network)
	vxlanAddr, err := h.runPlaypenOut(ctx, allocated, "ip", "addr", "show", "dev", cfg.VXLANInterface)
	if err != nil {
		h.t.Fatalf("show vxlan address for %s: %v", cfg.VXLANInterface, err)
	}

	if !strings.Contains(vxlanAddr, guestGateway) {
		h.t.Fatalf("vxlan interface %s missing guest gateway %s:\n%s", cfg.VXLANInterface, guestGateway, vxlanAddr)
	}

}

func guestGatewayPrefix(t *testing.T, network operator.NetworkResponse) string {
	t.Helper()

	gatewayIP, err := netip.ParseAddr(network.GatewayIPv4)
	if err != nil {
		t.Fatalf("parse gateway IPv4 %q: %v", network.GatewayIPv4, err)
	}

	return netip.PrefixFrom(gatewayIP, subnetMaskPrefixLength(t, network.SubnetMask)).String()
}

func guestSubnetCIDR(t *testing.T, network operator.NetworkResponse) string {
	t.Helper()

	guestIP, err := netip.ParseAddr(network.GuestIPv4)
	if err != nil {
		t.Fatalf("parse guest IPv4 %q: %v", network.GuestIPv4, err)
	}

	return netip.PrefixFrom(guestIP, subnetMaskPrefixLength(t, network.SubnetMask)).Masked().String()
}

func subnetMaskPrefixLength(t *testing.T, subnetMask string) int {
	t.Helper()

	mask := net.ParseIP(subnetMask).To4()
	if mask == nil {
		t.Fatalf("parse subnet mask %q", subnetMask)
	}

	ones, bits := net.IPMask(mask).Size()
	if bits != 32 {
		t.Fatalf("subnet mask %q is not contiguous", subnetMask)
	}

	return ones
}

func (h *harness) verifyTunnelConnectivity(ctx context.Context, allocated *playpenclient.Playpen) {
	h.t.Helper()

	readyURL := strings.TrimRight(allocated.Metadata.Redfish["url"], "/") + "/readyz"
	if _, err := url.ParseRequestURI(readyURL); err != nil {
		h.t.Fatalf("parse redfish ready URL %q: %v", readyURL, err)
	}

	if err := wait(ctx, 45*time.Second, func(ctx context.Context) (bool, error) {
		cmd, err := allocated.Command(ctx, os.Args[0], "-test.run", "^TestPlaypenRedfishReadyProbe$", "-test.count=1")
		if err != nil {
			return false, err
		}

		cmd.Dir = h.repoRoot
		cmd.Env = append(
			os.Environ(),
			"PLAYPEN_REDFISH_PROBE_URL="+readyURL,
			"PLAYPEN_REDFISH_PROBE_CERT_PEM="+strings.TrimSpace(allocated.Metadata.Redfish["certPEM"]),
		)

		_, err = h.runPlaypenCmdOut(cmd)
		if err != nil {
			return false, nil
		}

		return true, nil
	}); err != nil {
		h.dumpTunnelDiagnostics(context.Background(), allocated)
		h.t.Fatalf("verify tunnel connectivity to %s: %v", readyURL, err)
	}
}

func (h *harness) dumpTunnelDiagnostics(ctx context.Context, allocated *playpenclient.Playpen) {
	cfg := allocated.TunnelConfig()
	for _, args := range [][]string{
		{"route", "get", allocated.Metadata.VXLAN.ServerAddress},
		{"addr", "show", "dev", "unbounded0"},
		{"-d", "link", "show", "dev", cfg.VXLANInterface},
	} {
		out, err := h.runPlaypenOut(ctx, allocated, "ip", args...)
		if err != nil {
			h.t.Logf("ip %s failed: %v", strings.Join(args, " "), err)
			continue
		}

		h.t.Logf("ip %s\n%s", strings.Join(args, " "), out)
	}

	for _, args := range [][]string{
		{"get", "pod", allocated.Metadata.Pod.Name, "-n", namespace, "-o", "yaml"},
		{"logs", "-n", namespace, allocated.Metadata.Pod.Name, "--tail=200"},
	} {
		out, err := h.runOut(ctx, "kubectl", args...)
		if err != nil {
			h.t.Logf("kubectl %s failed: %v", strings.Join(args, " "), err)
			continue
		}

		h.t.Logf("kubectl %s\n%s", strings.Join(args, " "), out)
	}
}

func (h *harness) dumpHostUnboundedNetDiagnostics(ctx context.Context, runnerVXLANAddress string) {
	for _, args := range [][]string{
		{"ip", "addr", "show", "dev", "unbounded0"},
		{"ip", "route", "get", runnerVXLANAddress},
		{"wg", "show"},
	} {
		out, err := h.runOut(ctx, "sudo", append([]string{"-n"}, args...)...)
		if err != nil {
			h.t.Logf("sudo %s failed: %v", strings.Join(args, " "), err)
			continue
		}

		h.t.Logf("sudo %s\n%s", strings.Join(args, " "), out)
	}

	logPath := filepath.Join(h.artifacts, "external-unbounded-net-node.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		h.t.Logf("read %s failed: %v", logPath, err)
		return
	}

	h.t.Logf("external unbounded-net-node log\n%s", string(data))
}

func TestPlaypenRedfishReadyProbe(t *testing.T) {
	readyURL := os.Getenv("PLAYPEN_REDFISH_PROBE_URL")
	certPEM := os.Getenv("PLAYPEN_REDFISH_PROBE_CERT_PEM")
	if readyURL == "" && certPEM == "" {
		t.Skip("helper process only")
	}

	if readyURL == "" || certPEM == "" {
		t.Fatal("PLAYPEN_REDFISH_PROBE_URL and PLAYPEN_REDFISH_PROBE_CERT_PEM are required")
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(certPEM)) {
		t.Fatal("invalid Redfish certificate PEM")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, readyURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // Test cleanup only.

	io.Copy(io.Discard, resp.Body) //nolint:errcheck // Test response drain only.
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func (h *harness) teardown(ctx context.Context) {
	if err := h.run(ctx, "kind", "delete", "cluster", "--name", clusterName); err != nil {
		h.t.Logf("kind delete cluster: %v", err)
	}
}

func (h *harness) dumpDiagnostics(ctx context.Context) {
	for _, args := range [][]string{
		{"get", "pods", "-n", netNamespace, "-o", "wide"},
		{"describe", "pods", "-n", netNamespace},
		{"logs", "-n", netNamespace, "deployment/unbounded-net-controller", "--tail=200"},
		{"logs", "-n", netNamespace, "daemonset/unbounded-net-node", "--tail=200"},
		{"get", "sites,sitepeerings,sitenodeslices", "-o", "yaml"},
		{"get", "nodes", "-o", "yaml"},
		{"get", "pods", "-n", namespace, "-o", "wide"},
		{"describe", "pods", "-n", namespace},
		{"logs", "-n", namespace, "deployment/playpen-operator"},
		{"logs", "-n", namespace, "-l", "app.kubernetes.io/name=playpen-runner", "--tail=200"},
		{"get", "apiservice", "v1alpha1.playpen.unbounded-cloud.io", "-o", "yaml"},
	} {
		out, err := h.runOut(ctx, "kubectl", args...)
		if err != nil {
			h.t.Logf("kubectl %s failed: %v", strings.Join(args, " "), err)
			continue
		}

		h.t.Logf("kubectl %s\n%s", strings.Join(args, " "), out)
	}
}

func (h *harness) run(ctx context.Context, name string, args ...string) error {
	_, err := h.runCaptured(ctx, name, nil, args...)

	return err
}

func (h *harness) runOut(ctx context.Context, name string, args ...string) (string, error) {
	out, err := h.runCaptured(ctx, name, nil, args...)

	return out, err
}

func (h *harness) runWithInput(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	_, err := h.runCaptured(ctx, name, stdin, args...)

	return err
}

func (h *harness) runPlaypenOut(ctx context.Context, allocated *playpenclient.Playpen, name string, args ...string) (string, error) {
	h.t.Helper()

	cmd, err := allocated.Command(ctx, name, args...)
	if err != nil {
		return "", err
	}

	cmd.Dir = h.repoRoot

	return h.runPlaypenCmdOut(cmd)
}

func (h *harness) runPlaypenCmdOut(cmd *exec.Cmd) (string, error) {
	h.t.Helper()

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", cmd.Path, strings.Join(cmd.Args[1:], " "), err, out.String())
	}

	return out.String(), nil
}

func (h *harness) runCaptured(ctx context.Context, name string, stdin io.Reader, args ...string) (string, error) {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Stdin = stdin

	var out bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}

	return out.String(), nil
}

func wait(ctx context.Context, timeout time.Duration, condition func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		ok, err := condition(ctx)
		if ok || err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func goArchForDocker(arch string) string {
	switch arch {
	case "amd64", "arm64":
		return arch
	case "arm":
		return "arm/v7"
	default:
		return arch
	}
}
