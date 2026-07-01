//go:build e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metalman

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	api_meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	metalredfish "github.com/Azure/unbounded/internal/metalman/redfish"
	playpenclient "github.com/Azure/unbounded/internal/playpen/client"
	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	site          = "smoke"
	machineName   = "smoke-node"
	nodeNamespace = "default"
	nodeLabelKey  = "unbounded-cloud.io/smoke-test"
	nodeLabelVal  = "metalman"

	bmcSecretName = "metalman-smoke-bmc"
	bmcSecretKey  = "password"

	agentTarballName = "unbounded-agent-linux-amd64.tar.gz"

	artifactModeGHCR  = "ghcr"
	artifactModeLocal = "local"
)

type harness struct {
	t *testing.T

	repoRoot  string
	artifacts string
	keep      bool

	playpenKubeconfig string
	targetKubeconfig  string
	apiserverURL      string
	apiserverProxyTo  string
	artifactMode      string

	hostImage       string
	netbootImage    string
	agentImage      string
	agentGuestImage string

	targetClient ctrlclient.Client
	targetKube   kubernetes.Interface

	playpen        *playpenclient.Playpen
	consoleCancel  context.CancelFunc
	processCancels []context.CancelFunc
	processes      []*processHandle
	registryName   string
}

type processHandle struct {
	name   string
	done   chan error
	exited bool
	err    error
}

func TestPlaypenMetalmanSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	h := newHarness(t)
	h.checkPrereqs()
	h.initTargetClients()
	h.applyMetalmanPrereqs(ctx)
	h.cleanupOldResources(ctx)

	if !h.keep {
		t.Cleanup(func() { h.cleanupResources(context.Background()) })
	}
	t.Cleanup(func() { h.cleanupProcesses(context.Background()) })

	h.buildBinaries(ctx)
	h.packageAgentTarball()
	h.allocatePlaypen(ctx)
	h.prepareImages(ctx)
	h.startAPIServerProxy(ctx)
	h.startAgentServer(ctx)
	h.startMetalman(ctx)
	h.createBMCSecret(ctx)
	h.createMachine(ctx)

	cliOutput := h.replaceMachine(ctx)
	if !strings.Contains(cliOutput, "Condition CloudInitDone: True/Succeeded") {
		t.Fatalf("kubectl-unbounded output did not report CloudInitDone success:\n%s", cliOutput)
	}

	h.waitMachineCloudInit(ctx)
	h.waitNodeReady(ctx, machineName)
	h.assertNodeLabel(ctx)

	initialBootID := h.nodeBootID(ctx, machineName)

	h.runHostPowerOff(ctx)
	h.waitRedfishPowerState(ctx, metalredfish.PowerOff, 5*time.Minute)

	h.runHostPowerOn(ctx)
	h.waitRedfishPowerState(ctx, metalredfish.PowerOn, 5*time.Minute)
	h.waitNodeReady(ctx, machineName)
	bootIDAfterPowerOn := h.waitNodeBootIDChanged(ctx, machineName, initialBootID)

	h.runSelectorHostReboot(ctx)
	h.waitRedfishPowerState(ctx, metalredfish.PowerOn, 5*time.Minute)
	h.waitNodeReady(ctx, machineName)
	h.waitNodeBootIDChanged(ctx, machineName, bootIDAfterPowerOn)
}

func TestPrintPlaypenGateway(t *testing.T) {
	if os.Getenv("METALMAN_E2E_PRINT_PLAYPEN_GATEWAY") != "1" {
		t.Skip("set METALMAN_E2E_PRINT_PLAYPEN_GATEWAY=1 to allocate Playpen and print its guest gateway")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	playpenKubeconfig := envDefault("PLAYPEN_KUBECONFIG", filepath.Join(homeDir(t), ".kube", "config.bench"))
	playpenREST, err := clientcmd.BuildConfigFromFlags("", playpenKubeconfig)
	if err != nil {
		t.Fatalf("load Playpen kubeconfig: %v", err)
	}

	c, err := playpenclient.New(playpenclient.Config{RESTConfig: playpenREST})
	if err != nil {
		t.Fatalf("create Playpen client: %v", err)
	}

	p, err := c.Allocate(ctx, playpenclient.AllocateOptions{Architecture: runner.ArchitectureAMD64})
	if err != nil {
		t.Fatalf("allocate Playpen from %s: %v", playpenKubeconfig, err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		if err := p.Close(closeCtx); err != nil {
			t.Logf("close playpen: %v", err)
		}
	}()

	fmt.Printf("METALMAN_E2E_PLAYPEN_GATEWAY=%s\n", p.Metadata.Network.GatewayIPv4)
}

func TestDebugPlaypenReachability(t *testing.T) {
	if os.Getenv("METALMAN_E2E_DEBUG_PLAYPEN") != "1" {
		t.Skip("set METALMAN_E2E_DEBUG_PLAYPEN=1 to debug Playpen reachability")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := &harness{t: t, repoRoot: repoRoot(t), artifacts: t.TempDir(), playpenKubeconfig: envDefault("PLAYPEN_KUBECONFIG", filepath.Join(homeDir(t), ".kube", "config.bench"))}
	h.configurePlaypenTunnel(ctx)
	redfishURL := strings.TrimRight(h.playpen.Metadata.Redfish["url"], "/") + "/redfish/v1/"
	redfishHost := ""
	redfishPort := ""
	if parsed, err := url.Parse(redfishURL); err == nil {
		redfishHost = parsed.Hostname()
		redfishPort = parsed.Port()
	}

	t.Logf("Playpen metadata: %+v", h.playpen.Metadata)
	t.Logf("Playpen tunnel config: %+v", h.playpen.TunnelConfig())
	for _, args := range [][]string{
		{"--kubeconfig", h.playpenKubeconfig, "-n", h.playpen.Metadata.Pod.Namespace, "get", "pod", h.playpen.Metadata.Pod.Name, "-o", "yaml"},
		{"--kubeconfig", h.playpenKubeconfig, "-n", h.playpen.Metadata.Pod.Namespace, "get", "svc", "-o", "wide"},
		{"--kubeconfig", h.playpenKubeconfig, "-n", h.playpen.Metadata.Pod.Namespace, "logs", h.playpen.Metadata.Pod.Name, "--tail=200"},
		{"--kubeconfig", h.playpenKubeconfig, "-n", h.playpen.Metadata.Pod.Namespace, "exec", h.playpen.Metadata.Pod.Name, "--", "sh", "-c", "ip addr; ip route; wg show; (ss -lntu || netstat -lntu || true)"},
	} {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		out, err := h.runCmdOut(cmd)
		t.Logf("$ kubectl %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Logf("kubectl %s failed: %v", strings.Join(args, " "), err)
		}
	}

	var failed []string
	for _, args := range [][]string{
		{"ip", "addr"},
		{"ip", "route"},
		{"wg", "show"},
		{"python3", "-c", tcpProbePython(), redfishHost, redfishPort},
		{"python3", "-c", httpProbePython(), redfishURL},
	} {
		if len(args) > 2 && args[len(args)-1] == "" {
			t.Logf("skip %s: missing Redfish host or port", strings.Join(args, " "))
			continue
		}
		cmd, err := h.playpen.Command(ctx, args[0], args[1:]...)
		if err != nil {
			t.Fatalf("build %v: %v", args, err)
		}
		out, err := h.runCmdOut(cmd)
		t.Logf("$ %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", strings.Join(args, " "), err))
		}
	}
	if len(failed) > 0 {
		t.Fatalf("Playpen reachability checks failed:\n%s", strings.Join(failed, "\n"))
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	repoRoot := repoRoot(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	artifacts := filepath.Join(wd, ".artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	playpenKubeconfig := envDefault("PLAYPEN_KUBECONFIG", filepath.Join(homeDir(t), ".kube", "config.bench"))
	targetKubeconfig := strings.TrimSpace(os.Getenv("METALMAN_E2E_KUBECONFIG"))
	if targetKubeconfig == "" {
		targetKubeconfig = strings.TrimSpace(os.Getenv("KUBECONFIG"))
	}
	if targetKubeconfig == "" {
		targetKubeconfig = filepath.Join(homeDir(t), ".kube", "config")
	}

	apiserverURL := strings.TrimSpace(os.Getenv("METALMAN_E2E_APISERVER_URL"))

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("METALMAN_E2E_ARTIFACT_MODE")))
	if mode == "" {
		if allEnvSet("METALMAN_E2E_HOST_IMAGE", "METALMAN_E2E_NETBOOT_IMAGE", "METALMAN_E2E_AGENT_IMAGE") {
			mode = artifactModeGHCR
		} else {
			mode = artifactModeLocal
		}
	}
	if mode != artifactModeGHCR && mode != artifactModeLocal {
		t.Fatalf("METALMAN_E2E_ARTIFACT_MODE must be %q or %q", artifactModeGHCR, artifactModeLocal)
	}

	return &harness{
		t:                 t,
		repoRoot:          repoRoot,
		artifacts:         artifacts,
		keep:              os.Getenv("E2E_KEEP") == "1",
		playpenKubeconfig: playpenKubeconfig,
		targetKubeconfig:  targetKubeconfig,
		apiserverURL:      apiserverURL,
		apiserverProxyTo:  strings.TrimSpace(os.Getenv("METALMAN_E2E_APISERVER_PROXY_TARGET")),
		artifactMode:      mode,
		hostImage:         strings.TrimSpace(os.Getenv("METALMAN_E2E_HOST_IMAGE")),
		netbootImage:      strings.TrimSpace(os.Getenv("METALMAN_E2E_NETBOOT_IMAGE")),
		agentImage:        strings.TrimSpace(os.Getenv("METALMAN_E2E_AGENT_IMAGE")),
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func (h *harness) checkPrereqs() {
	h.t.Helper()

	required := []string{"bridge", "go", "ip", "iptables", "kubectl", "python3", "tar", "wg"}
	if h.artifactMode == artifactModeLocal {
		required = append(required, "docker")
	}
	for _, bin := range required {
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

	if _, err := os.Stat(h.playpenKubeconfig); err != nil {
		h.t.Fatalf("PLAYPEN_KUBECONFIG %q is required and must point at Playpen: %v", h.playpenKubeconfig, err)
	}
	if _, err := os.Stat(h.targetKubeconfig); err != nil {
		h.t.Fatalf("target kubeconfig %q is required; set METALMAN_E2E_KUBECONFIG or KUBECONFIG: %v", h.targetKubeconfig, err)
	}

	if h.artifactMode == artifactModeGHCR {
		if h.hostImage == "" || h.netbootImage == "" || h.agentImage == "" {
			h.t.Fatalf("GHCR artifact mode requires METALMAN_E2E_HOST_IMAGE, METALMAN_E2E_NETBOOT_IMAGE, and METALMAN_E2E_AGENT_IMAGE")
		}
		return
	}

	if err := h.run(context.Background(), "docker", "info"); err != nil {
		h.t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}
}

func (h *harness) initTargetClients() {
	h.t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", h.targetKubeconfig)
	if err != nil {
		h.t.Fatalf("load target kubeconfig: %v", err)
	}

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha3.AddToScheme(scheme))

	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		h.t.Fatalf("create target client: %v", err)
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		h.t.Fatalf("create target kubernetes client: %v", err)
	}

	h.targetClient = c
	h.targetKube = kube

	if _, err := kube.Discovery().ServerVersion(); err != nil {
		h.t.Fatalf("target cluster is not reachable with %s: %v", h.targetKubeconfig, err)
	}
}

func (h *harness) applyMetalmanPrereqs(ctx context.Context) {
	h.t.Helper()

	rendered := h.renderMetalmanPrereqs(ctx)
	h.kubectl(ctx, "apply", "-f", filepath.Join(rendered, "01-namespace.yaml"))
	h.kubectl(ctx, "apply", "-f", filepath.Join(rendered, "crd"))
	h.kubectl(ctx, "apply", "-f", filepath.Join(rendered, "06-metalman-rbac.yaml"))
}

func (h *harness) renderMetalmanPrereqs(ctx context.Context) string {
	h.t.Helper()

	rendered := filepath.Join(h.artifacts, "rendered")
	if err := os.RemoveAll(rendered); err != nil {
		h.t.Fatalf("clean rendered manifests: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rendered, "crd"), 0o755); err != nil {
		h.t.Fatalf("mkdir rendered manifests: %v", err)
	}

	templatesDir := filepath.Join(h.artifacts, "manifest-templates")
	if err := os.RemoveAll(templatesDir); err != nil {
		h.t.Fatalf("clean manifest templates: %v", err)
	}
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		h.t.Fatalf("mkdir manifest templates: %v", err)
	}
	for _, name := range []string{"01-namespace.yaml.tmpl", "06-metalman-rbac.yaml.tmpl"} {
		data, err := os.ReadFile(filepath.Join(h.repoRoot, "deploy", "machina", name))
		if err != nil {
			h.t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(templatesDir, name), data, 0o644); err != nil {
			h.t.Fatalf("write template %s: %v", name, err)
		}
	}

	h.runOrFatal(ctx, "render metalman manifests", "go", "run", "./hack/cmd/render-manifests", "--templates-dir", templatesDir, "--output-dir", rendered, "--set", "Namespace=unbounded-kube")

	crds, err := filepath.Glob(filepath.Join(h.repoRoot, "deploy", "machina", "crd", "*.yaml"))
	if err != nil {
		h.t.Fatalf("glob machina CRDs: %v", err)
	}
	for _, src := range crds {
		dst := filepath.Join(rendered, "crd", filepath.Base(src))
		data, err := os.ReadFile(src)
		if err != nil {
			h.t.Fatalf("read CRD %s: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			h.t.Fatalf("write CRD %s: %v", dst, err)
		}
	}

	return rendered
}

func (h *harness) cleanupOldResources(ctx context.Context) {
	h.t.Helper()

	h.cleanupResources(ctx)
}

func (h *harness) cleanupResources(ctx context.Context) {
	if h.targetClient == nil {
		return
	}

	for _, name := range []string{"smoke-host-poweroff", "smoke-host-poweron", "smoke-selector-host-reboot"} {
		h.deleteMachineOperation(ctx, name)
	}
	h.deleteReplaceOperations(ctx)
	h.deleteMachine(ctx, machineName)
	h.deleteNode(ctx, machineName)
	h.deleteSecret(ctx, nodeNamespace, bmcSecretName)
}

func (h *harness) buildBinaries(ctx context.Context) {
	h.t.Helper()

	h.runOrFatal(ctx, "build metalman", "go", "build", "-ldflags", os.Getenv("METALMAN_LDFLAGS"), "-o", filepath.Join(h.repoRoot, "bin", "metalman"), "./cmd/metalman/main.go")
	h.runOrFatal(ctx, "build kubectl-unbounded", "go", "build", "-ldflags", os.Getenv("KUBECTL_UNBOUNDED_LDFLAGS"), "-o", filepath.Join(h.repoRoot, "bin", "kubectl-unbounded"), "./cmd/kubectl-unbounded/main.go")
	h.runOrFatalWithEnv(ctx, "build unbounded-agent", []string{"GOOS=linux", "GOARCH=amd64"}, "go", "build", "-ldflags", os.Getenv("STAMP_LDFLAGS"), "-o", filepath.Join(h.repoRoot, "bin", "unbounded-agent"), "./cmd/agent/main.go")
}

func (h *harness) packageAgentTarball() {
	h.t.Helper()

	binPath := filepath.Join(h.repoRoot, "bin", "unbounded-agent")
	bin, err := os.ReadFile(binPath)
	if err != nil {
		h.t.Fatalf("read unbounded-agent binary: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "unbounded-agent", Mode: 0o755, Size: int64(len(bin))}); err != nil {
		h.t.Fatalf("write agent tar header: %v", err)
	}
	if _, err := tw.Write(bin); err != nil {
		h.t.Fatalf("write agent tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		h.t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		h.t.Fatalf("close gzip writer: %v", err)
	}

	path := filepath.Join(h.artifacts, agentTarballName)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
}

func (h *harness) prepareImages(ctx context.Context) {
	h.t.Helper()

	if h.artifactMode == artifactModeGHCR {
		h.agentGuestImage = h.agentImage
		return
	}
	if h.playpen == nil {
		h.t.Fatal("Playpen must be allocated before preparing local image refs")
	}

	registry := strings.TrimSpace(os.Getenv("METALMAN_E2E_LOCAL_REGISTRY"))
	if registry == "" {
		registry = "localhost:5555"
	}
	port := registryPort(h.t, registry)
	guestRegistry := h.playpen.Metadata.Network.GatewayIPv4 + ":" + port

	hostImage := registry + "/unbounded/host-ubuntu2404:smoke"
	netbootImage := registry + "/unbounded/netboot:smoke"
	agentImage := registry + "/unbounded/agent-ubuntu2404:smoke"
	h.hostImage = guestRegistry + "/unbounded/host-ubuntu2404:smoke"
	h.netbootImage = guestRegistry + "/unbounded/netboot:smoke"
	h.agentImage = guestRegistry + "/unbounded/agent-ubuntu2404:smoke"
	h.agentGuestImage = h.agentImage
	h.registryName = "metalman-e2e-registry"

	if err := h.run(ctx, "docker", "inspect", h.registryName); err != nil {
		h.runOrFatal(ctx, "start local registry", "docker", "run", "-d", "--restart=always", "-p", port+":5000", "--name", h.registryName, "registry:2")
	}
	h.t.Cleanup(func() {
		if !h.keep {
			h.run(context.Background(), "docker", "rm", "-f", h.registryName) //nolint:errcheck // Best-effort cleanup.
		}
	})
	h.runOrFatal(ctx, "build host image", "docker", "build", "-t", hostImage, "-f", "images/host-ubuntu2404/Containerfile", ".")
	h.runOrFatal(ctx, "build netboot image", "docker", "build", "-t", netbootImage, "-f", "images/netboot/Containerfile", ".")
	h.runOrFatal(ctx, "build agent image", "docker", "build", "-t", agentImage, "-f", "images/agent-ubuntu2404/Containerfile", "images/agent-ubuntu2404")
	h.runOrFatal(ctx, "push host image", "docker", "push", hostImage)
	h.runOrFatal(ctx, "push netboot image", "docker", "push", netbootImage)
	h.runOrFatal(ctx, "push agent image", "docker", "push", agentImage)
	h.startRegistryForwarder(ctx, port)
}

func (h *harness) startRegistryForwarder(ctx context.Context, port string) {
	h.t.Helper()

	hostIP := addressFromCIDR(h.t, h.playpen.TunnelConfig().ManagementHostAddress)
	cmd, err := h.playpen.Command(ctx, "python3", "-c", tcpForwarderPython(), h.playpen.Metadata.Network.GatewayIPv4, port, hostIP, port)
	if err != nil {
		h.t.Fatalf("build registry forwarder command: %v", err)
	}
	h.startProcess(ctx, "registry-forwarder", cmd)

	h.waitPlaypenHTTP(ctx, "registry forwarder", fmt.Sprintf("http://%s:%s/v2/", h.playpen.Metadata.Network.GatewayIPv4, port))
}

func (h *harness) allocatePlaypen(ctx context.Context) {
	h.t.Helper()
	h.configurePlaypenTunnel(ctx)
	h.waitPlaypenRedfish(ctx)

	if h.artifactMode != artifactModeLocal {
		h.agentGuestImage = h.agentImage
	}
}

func (h *harness) configurePlaypenTunnel(ctx context.Context) {
	h.t.Helper()

	playpenREST, err := clientcmd.BuildConfigFromFlags("", h.playpenKubeconfig)
	if err != nil {
		h.t.Fatalf("load Playpen kubeconfig: %v", err)
	}

	c, err := playpenclient.New(playpenclient.Config{RESTConfig: playpenREST})
	if err != nil {
		h.t.Fatalf("create Playpen client: %v", err)
	}

	p, err := c.Allocate(ctx, playpenclient.AllocateOptions{Architecture: runner.ArchitectureAMD64})
	if err != nil {
		h.t.Fatalf("allocate Playpen from %s: %v", h.playpenKubeconfig, err)
	}
	h.playpen = p

	h.t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := p.Close(closeCtx); err != nil {
			h.t.Logf("close playpen: %v", err)
		}
	})

	if err := p.ConfigureTunnel(ctx); err != nil {
		h.t.Fatalf("configure Playpen tunnel: %v", err)
	}
	if h.apiserverURL == "" {
		h.apiserverURL = fmt.Sprintf("https://%s:6443", p.Metadata.Network.GatewayIPv4)
	}
}

func (h *harness) waitPlaypenRedfish(ctx context.Context) {
	h.t.Helper()
	h.waitPlaypenHTTP(ctx, "Playpen Redfish", strings.TrimRight(h.playpen.Metadata.Redfish["url"], "/")+"/redfish/v1/")
}

func (h *harness) startAPIServerProxy(ctx context.Context) {
	h.t.Helper()

	proxyTarget := h.apiserverProxyTo
	if proxyTarget == "" {
		parsed, err := url.Parse(h.apiserverURL)
		if err != nil {
			h.t.Fatalf("parse METALMAN_E2E_APISERVER_URL %q: %v", h.apiserverURL, err)
		}
		if parsed.Hostname() != h.playpen.Metadata.Network.GatewayIPv4 {
			return
		}

		proxyTarget = firstClusterServer(h.t, h.targetKubeconfig)
	}

	target, err := url.Parse(proxyTarget)
	if err != nil {
		h.t.Fatalf("parse METALMAN_E2E_APISERVER_PROXY_TARGET %q: %v", proxyTarget, err)
	}
	targetHost := target.Hostname()
	targetPort := target.Port()
	if targetHost == "" || targetPort == "" {
		h.t.Fatalf("API server proxy target %q must include host and port", proxyTarget)
	}
	if targetHost == "127.0.0.1" || targetHost == "localhost" || targetHost == "::1" {
		hostIP := addressFromCIDR(h.t, h.playpen.TunnelConfig().ManagementHostAddress)
		hostProxyPort := envDefault("METALMAN_E2E_APISERVER_HOST_PROXY_PORT", "16443")
		hostCmd := exec.CommandContext(ctx, "python3", "-c", tcpForwarderPython(), hostIP, hostProxyPort, targetHost, targetPort)
		h.startProcess(ctx, "apiserver-host-proxy", hostCmd)
		h.waitTCP(ctx, "API server host proxy", net.JoinHostPort(hostIP, hostProxyPort))

		targetHost = hostIP
		targetPort = hostProxyPort
	}

	listen, err := url.Parse(h.apiserverURL)
	if err != nil {
		h.t.Fatalf("parse METALMAN_E2E_APISERVER_URL %q: %v", h.apiserverURL, err)
	}
	listenHost := listen.Hostname()
	listenPort := listen.Port()
	if listenHost == "" || listenPort == "" {
		h.t.Fatalf("METALMAN_E2E_APISERVER_URL %q must include host and port", h.apiserverURL)
	}

	cmd, err := h.playpen.Command(ctx, "python3", "-c", tcpForwarderPython(), listenHost, listenPort, targetHost, targetPort)
	if err != nil {
		h.t.Fatalf("build API server proxy command: %v", err)
	}
	h.startProcess(ctx, "apiserver-proxy", cmd)
	h.waitPlaypenTCP(ctx, "API server proxy", net.JoinHostPort(listenHost, listenPort))
}

func (h *harness) startConsoleLog(ctx context.Context) {
	h.t.Helper()

	path := filepath.Join(h.artifacts, "serial.log")
	file, err := os.Create(path)
	if err != nil {
		h.t.Fatalf("create serial log: %v", err)
	}
	h.t.Cleanup(func() { file.Close() }) //nolint:errcheck // Test artifact cleanup.

	consoleCtx, cancel := context.WithCancel(ctx)
	h.consoleCancel = cancel
	errCh := h.playpen.StreamConsoleLogs(consoleCtx, file)
	h.t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				h.t.Logf("serial console stream ended: %v", err)
			}
		case <-time.After(2 * time.Second):
		}
	})
}

func (h *harness) startAgentServer(ctx context.Context) {
	h.t.Helper()

	cmd, err := h.playpen.Command(ctx, "python3", "-m", "http.server", "8881", "--bind", h.playpen.Metadata.Network.GatewayIPv4, "--directory", h.artifacts)
	if err != nil {
		h.t.Fatalf("build agent HTTP server command: %v", err)
	}
	h.startProcess(ctx, "agent-http", cmd)

	url := fmt.Sprintf("http://%s:8881/%s", h.playpen.Metadata.Network.GatewayIPv4, agentTarballName)
	h.waitPlaypenHTTP(ctx, "agent tarball server", url)
}

func (h *harness) startMetalman(ctx context.Context) {
	h.t.Helper()

	if h.playpen == nil {
		h.t.Fatal("Playpen must be allocated before starting metalman")
	}

	cacheDir := filepath.Join(h.artifacts, "metalman-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		h.t.Fatalf("mkdir metalman cache: %v", err)
	}

	cmd, err := h.playpen.Command(ctx,
		"env",
		"KUBECONFIG="+h.targetKubeconfig,
		"METALMAN_APISERVER_URL="+h.apiserverURL,
		filepath.Join(h.repoRoot, "bin", "metalman"),
		"serve-pxe",
		"--site", site,
		"--bind-address", h.playpen.Metadata.Network.GatewayIPv4,
		"--cache-dir", cacheDir,
		"--serve-url", fmt.Sprintf("http://%s:8880", h.playpen.Metadata.Network.GatewayIPv4),
		"--dhcp-interface", h.playpen.TunnelConfig().VXLANInterface,
		"--default-netboot-image", h.netbootImage,
		"--leader-elect-lease-duration", "5s",
		"--leader-elect-renew-deadline", "3s",
		"--leader-elect-retry-period", "1s",
		"--operation-poll-interval", "2s",
	)
	if err != nil {
		h.t.Fatalf("build metalman command: %v", err)
	}
	h.startProcess(ctx, "metalman", cmd)
	h.waitPlaypenHTTP(ctx, "metalman health", fmt.Sprintf("http://%s:8081/healthz", h.playpen.Metadata.Network.GatewayIPv4))
}

func (h *harness) createBMCSecret(ctx context.Context) {
	h.t.Helper()

	password := h.playpen.Metadata.Redfish["password"]
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bmcSecretName, Namespace: nodeNamespace}}
	if err := h.targetClient.Get(ctx, types.NamespacedName{Name: bmcSecretName, Namespace: nodeNamespace}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			h.t.Fatalf("get BMC secret: %v", err)
		}
		secret.Data = map[string][]byte{bmcSecretKey: []byte(password)}
		if err := h.targetClient.Create(ctx, secret); err != nil {
			h.t.Fatalf("create BMC secret: %v", err)
		}
		return
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[bmcSecretKey] = []byte(password)
	if err := h.targetClient.Update(ctx, secret); err != nil {
		h.t.Fatalf("update BMC secret: %v", err)
	}
}

func (h *harness) createMachine(ctx context.Context) {
	h.t.Helper()

	machine := &v1alpha3.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha3.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: machineName,
			Labels: map[string]string{
				v1alpha3.MachineSiteLabelKey: site,
			},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        h.hostImage,
				Architecture: v1alpha3.PXEArchitectureAMD64,
				NetbootImage: h.netbootImage,
				DHCPLeases: []v1alpha3.DHCPLease{{
					IPv4:       h.playpen.Metadata.Network.GuestIPv4,
					MAC:        h.playpen.Metadata.Network.GuestMAC,
					SubnetMask: h.playpen.Metadata.Network.SubnetMask,
					Gateway:    h.playpen.Metadata.Network.GatewayIPv4,
					DNS:        h.playpen.Metadata.Network.DNS,
				}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:      h.playpen.Metadata.Redfish["url"],
					Username: h.playpen.Metadata.Redfish["username"],
					DeviceID: redfishValue(h.playpen, "deviceID", "1"),
					PasswordRef: v1alpha3.SecretKeySelector{
						Name:      bmcSecretName,
						Namespace: nodeNamespace,
						Key:       bmcSecretKey,
					},
				},
			},
			Agent: &v1alpha3.AgentSpec{
				Image: h.agentGuestImage,
				URL:   fmt.Sprintf("http://%s:8881/%s", h.playpen.Metadata.Network.GatewayIPv4, agentTarballName),
			},
			Kubernetes: &v1alpha3.KubernetesSpec{
				NodeLabels: map[string]string{nodeLabelKey: nodeLabelVal},
			},
		},
	}

	if err := h.targetClient.Create(ctx, machine); err != nil {
		h.t.Fatalf("create Machine: %v", err)
	}
}

func (h *harness) replaceMachine(ctx context.Context) string {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, filepath.Join(h.repoRoot, "bin", "kubectl-unbounded"), "machine", "replace", machineName, "--force", "--ttl=3600")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.targetKubeconfig)
	out, err := h.runCmdOut(cmd)
	if err != nil {
		h.collectDiagnostics(context.Background())
		h.t.Fatalf("kubectl-unbounded machine replace failed: %v\n%s", err, out)
	}
	return out
}

func (h *harness) waitMachineCloudInit(ctx context.Context) {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		machine := &v1alpha3.Machine{}
		if err := h.targetClient.Get(ctx, types.NamespacedName{Name: machineName}, machine); err == nil {
			cond := api_meta.FindStatusCondition(machine.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
			if cond != nil {
				if cond.Status == metav1.ConditionTrue && cond.Reason == "Succeeded" {
					return
				}
				if cond.Status == metav1.ConditionFalse && cond.Reason == "Failed" {
					h.collectDiagnostics(context.Background())
					h.t.Fatalf("Machine CloudInitDone failed: %s", cond.Message)
				}
			}
		}
		h.checkProcesses()
		sleepOrDone(ctx, 2*time.Second)
	}

	h.collectDiagnostics(context.Background())
	h.t.Fatal("timed out waiting for Machine CloudInitDone=True/Succeeded")
}

func (h *harness) waitNodeReady(ctx context.Context, name string) {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		node, err := h.targetKube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil && nodeReady(node) {
			return
		}
		h.checkProcesses()
		sleepOrDone(ctx, 3*time.Second)
	}

	h.collectDiagnostics(context.Background())
	h.t.Fatalf("timed out waiting for Node %q Ready", name)
}

func (h *harness) assertNodeLabel(ctx context.Context) {
	h.t.Helper()

	node, err := h.targetKube.CoreV1().Nodes().Get(ctx, machineName, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("get Node %q: %v", machineName, err)
	}
	if got := node.Labels[nodeLabelKey]; got != nodeLabelVal {
		h.t.Fatalf("Node label %s = %q, want %q", nodeLabelKey, got, nodeLabelVal)
	}
}

func (h *harness) nodeBootID(ctx context.Context, name string) string {
	h.t.Helper()

	node, err := h.targetKube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("get Node boot ID: %v", err)
	}
	if node.Status.NodeInfo.BootID == "" {
		h.t.Fatalf("Node %q has empty boot ID", name)
	}
	return node.Status.NodeInfo.BootID
}

func (h *harness) waitNodeBootIDChanged(ctx context.Context, name, previous string) string {
	h.t.Helper()

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		node, err := h.targetKube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil && node.Status.NodeInfo.BootID != "" && node.Status.NodeInfo.BootID != previous {
			return node.Status.NodeInfo.BootID
		}
		h.checkProcesses()
		sleepOrDone(ctx, 3*time.Second)
	}

	h.collectDiagnostics(context.Background())
	h.t.Fatalf("timed out waiting for Node %q boot ID to change", name)
	return ""
}

func (h *harness) runHostPowerOff(ctx context.Context) {
	h.t.Helper()
	h.createAndWaitOperation(ctx, "smoke-host-poweroff", v1alpha3.MachineOperationSpec{MachineRef: machineName, OperationKind: v1alpha3.OperationHostPowerOff})
}

func (h *harness) runHostPowerOn(ctx context.Context) {
	h.t.Helper()
	h.createAndWaitOperation(ctx, "smoke-host-poweron", v1alpha3.MachineOperationSpec{MachineRef: machineName, OperationKind: v1alpha3.OperationHostPowerOn})
}

func (h *harness) runSelectorHostReboot(ctx context.Context) {
	h.t.Helper()
	h.createAndWaitOperation(ctx, "smoke-selector-host-reboot", v1alpha3.MachineOperationSpec{
		MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha3.MachineSiteLabelKey: site}},
		OperationKind:   v1alpha3.OperationHostReboot,
	})
}

func (h *harness) createAndWaitOperation(ctx context.Context, name string, spec v1alpha3.MachineOperationSpec) {
	h.t.Helper()

	ttl := int32(3600)
	spec.TTLSecondsAfterFinished = &ttl
	op := &v1alpha3.MachineOperation{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha3.GroupVersion.String(), Kind: "MachineOperation"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	if err := h.targetClient.Create(ctx, op); err != nil {
		h.t.Fatalf("create MachineOperation %s: %v", name, err)
	}

	h.waitOperationComplete(ctx, name)
}

func (h *harness) waitOperationComplete(ctx context.Context, name string) {
	h.t.Helper()

	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		op := &v1alpha3.MachineOperation{}
		err := h.targetClient.Get(ctx, types.NamespacedName{Name: name}, op)
		if err == nil {
			switch op.Status.Phase {
			case v1alpha3.OperationPhaseComplete:
				if !operationHasCompleteTarget(op, machineName) {
					h.t.Fatalf("MachineOperation %s completed without complete target for %s: %#v", name, machineName, op.Status.Targets)
				}
				return
			case v1alpha3.OperationPhaseFailed:
				h.collectDiagnostics(context.Background())
				h.t.Fatalf("MachineOperation %s failed: %s", name, op.Status.Message)
			}
		}
		h.checkProcesses()
		sleepOrDone(ctx, 2*time.Second)
	}

	h.collectDiagnostics(context.Background())
	h.t.Fatalf("timed out waiting for MachineOperation %s to complete", name)
}

func (h *harness) waitRedfishPowerState(ctx context.Context, want metalredfish.PowerState, timeout time.Duration) {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := h.playpenRedfishPowerState(ctx)
		if err == nil && state == want {
			return
		}
		h.checkProcesses()
		sleepOrDone(ctx, 2*time.Second)
	}

	h.collectDiagnostics(context.Background())
	h.t.Fatalf("timed out waiting for Redfish power state %s", want)
}

func (h *harness) playpenRedfishPowerState(ctx context.Context) (metalredfish.PowerState, error) {
	h.t.Helper()

	url := strings.TrimRight(h.playpen.Metadata.Redfish["url"], "/") + "/redfish/v1/Systems/" + redfishValue(h.playpen, "deviceID", "1")
	cmd, err := h.playpen.Command(ctx, "python3", "-c", redfishPowerStatePython(), url, h.playpen.Metadata.Redfish["username"], h.playpen.Metadata.Redfish["password"])
	if err != nil {
		return "", err
	}
	out, err := h.runCmdOut(cmd)
	if err != nil {
		return "", fmt.Errorf("query Redfish power state: %w: %s", err, out)
	}
	return metalredfish.PowerState(strings.TrimSpace(out)), nil
}

func (h *harness) startProcess(ctx context.Context, name string, cmd *exec.Cmd) {
	h.t.Helper()

	procCtx, cancel := context.WithCancel(ctx)
	h.processCancels = append(h.processCancels, cancel)
	cmd.Cancel = func() error {
		cancel()
		if cmd.Process != nil {
			return cmd.Process.Signal(os.Interrupt)
		}
		return nil
	}

	logPath := filepath.Join(h.artifacts, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		h.t.Fatalf("create %s log: %v", name, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close() //nolint:errcheck // Cleanup on failure.
		h.t.Fatalf("start %s: %v", name, err)
	}

	done := make(chan error, 1)
	proc := &processHandle{name: name, done: done}
	go func() {
		done <- cmd.Wait()
		logFile.Close() //nolint:errcheck // Test artifact cleanup.
	}()
	h.processes = append(h.processes, proc)

	go func() {
		<-procCtx.Done()
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt) //nolint:errcheck // Best effort shutdown.
		}
	}()

	h.t.Cleanup(func() {
		cancel()
		if !proc.exited {
			select {
			case err := <-done:
				proc.exited = true
				proc.err = err
			case <-time.After(5 * time.Second):
				if cmd.Process != nil {
					cmd.Process.Kill() //nolint:errcheck // Best effort cleanup.
				}
				proc.err = <-done
				proc.exited = true
			}
		}
		if proc.err != nil && ctx.Err() == nil {
			h.t.Logf("%s exited during cleanup: %v", name, proc.err)
		}
	})
}

func (h *harness) cleanupProcesses(context.Context) {
	if h.consoleCancel != nil {
		h.consoleCancel()
	}
	for _, cancel := range h.processCancels {
		cancel()
	}
}

func (h *harness) checkProcesses() {
	h.t.Helper()

	for _, proc := range h.processes {
		if !proc.exited {
			select {
			case err := <-proc.done:
				proc.exited = true
				proc.err = err
			default:
				continue
			}
		}

		if proc.err != nil {
			h.collectDiagnostics(context.Background())
			h.t.Fatalf("%s exited early: %v; see %s", proc.name, proc.err, filepath.Join(h.artifacts, proc.name+".log"))
		}
		h.collectDiagnostics(context.Background())
		h.t.Fatalf("%s exited early; see %s", proc.name, filepath.Join(h.artifacts, proc.name+".log"))
	}
}

func (h *harness) waitHTTP(ctx context.Context, name, url string) {
	h.t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body) //nolint:errcheck // Drain best effort.
				resp.Body.Close()              //nolint:errcheck // Close best effort.
				if resp.StatusCode >= 200 && resp.StatusCode < 500 {
					return
				}
			}
		}
		sleepOrDone(ctx, time.Second)
	}

	h.t.Fatalf("timed out waiting for %s at %s", name, url)
}

func (h *harness) waitPlaypenHTTP(ctx context.Context, name, url string) {
	h.t.Helper()

	var lastErr error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := h.playpen.Command(ctx, "python3", "-c", httpProbePython(), url)
		if err != nil {
			lastErr = err
		} else if out, runErr := h.runCmdOut(cmd); runErr == nil {
			return
		} else {
			lastErr = fmt.Errorf("%w: %s", runErr, strings.TrimSpace(out))
		}
		h.checkProcesses()
		sleepOrDone(ctx, time.Second)
	}

	h.t.Fatalf("timed out waiting for %s at %s: last error: %v", name, url, lastErr)
}

func (h *harness) waitTCP(ctx context.Context, name, address string) {
	h.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			conn.Close() //nolint:errcheck // Probe cleanup.
			return
		}
		h.checkProcesses()
		sleepOrDone(ctx, time.Second)
	}

	h.t.Fatalf("timed out waiting for %s at %s", name, address)
}

func (h *harness) waitPlaypenTCP(ctx context.Context, name, address string) {
	h.t.Helper()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		h.t.Fatalf("parse TCP probe address %q: %v", address, err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := h.playpen.Command(ctx, "python3", "-c", tcpProbePython(), host, port)
		if err == nil && h.runCmd(cmd) == nil {
			return
		}
		h.checkProcesses()
		sleepOrDone(ctx, time.Second)
	}

	h.t.Fatalf("timed out waiting for %s at %s", name, address)
}

func (h *harness) collectDiagnostics(ctx context.Context) {
	if h.targetClient == nil {
		return
	}

	diagPath := filepath.Join(h.artifacts, "diagnostics.txt")
	f, err := os.Create(diagPath)
	if err != nil {
		h.t.Logf("create diagnostics: %v", err)
		return
	}
	defer f.Close() //nolint:errcheck // Test artifact cleanup.

	commands := [][]string{
		{"get", "machines.unbounded-cloud.io", machineName, "-o", "yaml"},
		{"get", "machineoperations.unbounded-cloud.io", "-o", "wide"},
		{"get", "machineoperations.unbounded-cloud.io", "-o", "yaml"},
		{"describe", "node", machineName},
		{"get", "pods", "-A", "-o", "wide"},
		{"get", "events", "-A", "--sort-by=.lastTimestamp"},
	}
	for _, args := range commands {
		fmt.Fprintf(f, "\n$ kubectl %s\n", strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", h.targetKubeconfig}, args...)...)
		out, _ := cmd.CombinedOutput()
		f.Write(out) //nolint:errcheck // Best effort diagnostics.
	}

	if h.playpen != nil {
		state, err := h.playpenRedfishPowerState(ctx)
		fmt.Fprintf(f, "\nRedfish power state: %s err=%v\n", state, err)
	}

	if h.playpen != nil {
		data, _ := json.MarshalIndent(h.playpen.Metadata, "", "  ")
		fmt.Fprintf(f, "\nPlaypen metadata:\n%s\n", data)
	}
}

func (h *harness) kubectl(ctx context.Context, args ...string) {
	h.t.Helper()
	h.runOrFatal(ctx, "kubectl "+strings.Join(args, " "), "kubectl", append([]string{"--kubeconfig", h.targetKubeconfig}, args...)...)
}

func (h *harness) runOrFatal(ctx context.Context, label, name string, args ...string) {
	h.t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	if out, err := h.runCmdOut(cmd); err != nil {
		h.t.Fatalf("%s failed: %v\n%s", label, err, out)
	}
}

func (h *harness) runOrFatalWithEnv(ctx context.Context, label string, env []string, name string, args ...string) {
	h.t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = append(os.Environ(), env...)
	if out, err := h.runCmdOut(cmd); err != nil {
		h.t.Fatalf("%s failed: %v\n%s", label, err, out)
	}
}

func (h *harness) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	return h.runCmd(cmd)
}

func (h *harness) runCmd(cmd *exec.Cmd) error {
	_, err := h.runCmdOut(cmd)
	return err
}

func (h *harness) runCmdOut(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func (h *harness) deleteMachineOperation(ctx context.Context, name string) {
	op := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := h.targetClient.Delete(ctx, op); err != nil && !apierrors.IsNotFound(err) {
		h.t.Logf("delete MachineOperation %s: %v", name, err)
	}
}

func (h *harness) deleteReplaceOperations(ctx context.Context) {
	ops := &v1alpha3.MachineOperationList{}
	if err := h.targetClient.List(ctx, ops); err != nil {
		return
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if strings.HasPrefix(op.Name, machineName+"-replace-") {
			if err := h.targetClient.Delete(ctx, op); err != nil && !apierrors.IsNotFound(err) {
				h.t.Logf("delete MachineOperation %s: %v", op.Name, err)
			}
		}
	}
}

func (h *harness) deleteMachine(ctx context.Context, name string) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := h.targetClient.Delete(ctx, machine); err != nil && !apierrors.IsNotFound(err) {
		h.t.Logf("delete Machine %s: %v", name, err)
	}
}

func (h *harness) deleteNode(ctx context.Context, name string) {
	if err := h.targetKube.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		h.t.Logf("delete Node %s: %v", name, err)
	}
}

func (h *harness) deleteSecret(ctx context.Context, namespace, name string) {
	if err := h.targetKube.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		h.t.Logf("delete Secret %s/%s: %v", namespace, name, err)
	}
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func operationHasCompleteTarget(op *v1alpha3.MachineOperation, machine string) bool {
	for _, target := range op.Status.Targets {
		if target.MachineRef == machine && target.Phase == v1alpha3.OperationPhaseComplete {
			return true
		}
	}
	return false
}

func sleepOrDone(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func allEnvSet(keys ...string) bool {
	for _, key := range keys {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			return false
		}
	}
	return true
}

func homeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	return home
}

func redfishValue(p *playpenclient.Playpen, key, fallback string) string {
	if value := strings.TrimSpace(p.Metadata.Redfish[key]); value != "" {
		return value
	}
	return fallback
}

func addressFromCIDR(t *testing.T, value string) string {
	t.Helper()

	addr, err := netip.ParseAddr(value)
	if err == nil {
		return addr.String()
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		t.Fatalf("parse address %q: %v", value, err)
	}
	return prefix.Addr().String()
}

func firstClusterServer(t *testing.T, kubeconfig string) string {
	t.Helper()

	raw, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig %s: %v", kubeconfig, err)
	}
	if raw.CurrentContext != "" {
		if context := raw.Contexts[raw.CurrentContext]; context != nil {
			if cluster := raw.Clusters[context.Cluster]; cluster != nil && strings.TrimSpace(cluster.Server) != "" {
				return strings.TrimSpace(cluster.Server)
			}
		}
	}
	for _, cluster := range raw.Clusters {
		if strings.TrimSpace(cluster.Server) != "" {
			return strings.TrimSpace(cluster.Server)
		}
	}
	t.Fatalf("kubeconfig %s has no cluster server", kubeconfig)
	return ""
}

func rewriteRegistryHost(ref, host string) string {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return ref
	}
	registry := parts[0]
	if strings.Contains(registry, ".") || strings.Contains(registry, ":") || registry == "localhost" {
		if _, port, ok := strings.Cut(registry, ":"); ok {
			return host + ":" + port + "/" + parts[1]
		}
		return host + "/" + parts[1]
	}
	return ref
}

func registryPort(t *testing.T, registry string) string {
	t.Helper()

	host, port, err := net.SplitHostPort(registry)
	if err == nil {
		if host == "" || port == "" {
			t.Fatalf("registry %q must include host and port", registry)
		}
		return port
	}

	if strings.Contains(err.Error(), "missing port in address") {
		t.Fatalf("registry %q must include a port so it can be forwarded into Playpen", registry)
	}
	t.Fatalf("parse registry %q: %v", registry, err)
	return ""
}

func tcpForwarderPython() string {
	return `import socketserver, socket, sys, threading
listen_host=sys.argv[1]
listen_port=int(sys.argv[2])
target_host=sys.argv[3]
target_port=int(sys.argv[4])
class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address=True
class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        upstream=socket.create_connection((target_host,target_port))
        def pump(src,dst):
            try:
                while True:
                    data=src.recv(65536)
                    if not data:
                        break
                    dst.sendall(data)
            finally:
                try: dst.shutdown(socket.SHUT_WR)
                except OSError: pass
        t=threading.Thread(target=pump,args=(self.request,upstream),daemon=True)
        t.start()
        pump(upstream,self.request)
        upstream.close()
with Server((listen_host,listen_port), Handler) as server:
    server.serve_forever()
`
}

func httpProbePython() string {
	return `import ssl, sys, urllib.request
req=urllib.request.Request(sys.argv[1], method="GET")
context=ssl._create_unverified_context()
with urllib.request.urlopen(req, timeout=2, context=context) as resp:
    if resp.status < 200 or resp.status >= 500:
        raise SystemExit(1)
`
}

func tcpProbePython() string {
	return `import socket, sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=2):
    pass
`
}

func redfishPowerStatePython() string {
	return `import json, ssl, sys, urllib.request
url=sys.argv[1]
username=sys.argv[2]
password=sys.argv[3]
request=urllib.request.Request(url, method="GET")
if username:
    import base64
    token=base64.b64encode((username+":"+password).encode()).decode()
    request.add_header("Authorization", "Basic "+token)
context=ssl._create_unverified_context()
with urllib.request.urlopen(request, timeout=5, context=context) as resp:
    data=json.loads(resp.read().decode())
print(data.get("PowerState", ""))
`
}
