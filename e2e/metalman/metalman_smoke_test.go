//go:build e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metalman

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	internalkube "github.com/Azure/unbounded/internal/kube"
	metalredfish "github.com/Azure/unbounded/internal/metalman/redfish"
	playpenclient "github.com/Azure/unbounded/internal/playpen/client"
	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	siteName            = "smoke"
	machineName         = "smoke-node"
	redfishSecretName   = "bmc-pass"
	redfishSecretKey    = "password"
	nodeSmokeLabelKey   = "unbounded-cloud.io/smoke-test"
	nodeSmokeLabelValue = "metalman"
	fieldManager        = "metalman-smoke"
	agentArchiveName    = "unbounded-agent-linux-amd64.tar.gz"
	agentHTTPPort       = "8881"
	metalmanHTTPPort    = "8880"
	metalmanHealthPort  = "8085"
	testTimeout         = 45 * time.Minute
)

type harness struct {
	t         *testing.T
	repoRoot  string
	artifacts string
	private   string
	childKube string
	images    smokeImages

	childREST   *rest.Config
	childClient client.Client
	childset    kubernetes.Interface
}

type targetControlPlane struct {
	guestAPIServerURL string
}

type smokeImages struct {
	host    string
	netboot string
	agent   string
}

func TestMetalmanSmokeOnPlaypen(t *testing.T) {
	if os.Getenv("METALMAN_SMOKE_AGENT_HELPER") == "1" {
		runAgentHTTPHelper(t)

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	h := newHarness(t)
	h.checkPrereqs(ctx)
	h.buildBinaries(ctx)
	h.packageAgent(ctx)

	playpenREST, err := loadDefaultRESTConfig()
	if err != nil {
		t.Fatalf("load default kubeconfig: %v", err)
	}

	ppClient, err := playpenclient.New(playpenclient.Config{RESTConfig: playpenREST})
	if err != nil {
		t.Fatalf("create playpen client: %v", err)
	}
	playpenSet, err := kubernetes.NewForConfig(playpenREST)
	if err != nil {
		t.Fatalf("create playpen clientset: %v", err)
	}

	allocated, err := ppClient.Allocate(ctx, playpenclient.AllocateOptions{Architecture: runner.ArchitectureAMD64})
	if err != nil {
		t.Fatalf("allocate playpen runner: %v", err)
	}
	h.cleanupPlaypen(allocated)

	target := h.configureTargetControlPlane(ctx, ppClient, playpenREST)

	if err := h.initChildClients(); err != nil {
		t.Fatalf("create child cluster clients: %v", err)
	}
	if err := h.ensureClusterInfo(ctx); err != nil {
		t.Fatalf("ensure cluster-info ConfigMap: %v", err)
	}

	if err := h.applyChildManifests(ctx); err != nil {
		t.Fatalf("apply child manifests: %v", err)
	}

	h.configurePlaypenTunnel(ctx, playpenSet, allocated)

	h.startSerialLog(ctx, allocated)

	agentURL := "http://" + allocated.Metadata.Network.GatewayIPv4 + ":" + agentHTTPPort + "/" + agentArchiveName
	agentCmd := h.startAgentHTTPServer(ctx, allocated)
	metalmanCmd := h.startMetalman(ctx, allocated, target.guestAPIServerURL)

	h.cleanupChildResources(ctx)
	h.createRedfishSecret(ctx, allocated)
	h.createMachine(ctx, allocated, agentURL)

	h.waitForMetalmanReady(ctx, allocated)
	h.waitForPowerState(ctx, allocated, metalredfish.PowerOn)

	h.createMachineOperation(ctx, "host-replace", machinav1alpha3.OperationHostReplace, nil)
	h.waitMachineOperationComplete(ctx, "host-replace", machineName)
	h.waitMachineCondition(ctx, machinav1alpha3.MachineConditionCloudInitDone, metav1.ConditionTrue, "Succeeded")
	h.waitNodeReady(ctx, machineName)
	h.assertNodeLabel(ctx, machineName, nodeSmokeLabelKey, nodeSmokeLabelValue)
	bootID := h.waitNodeBootID(ctx, machineName)

	h.createMachineOperation(ctx, "host-power-off", machinav1alpha3.OperationHostPowerOff, nil)
	h.waitMachineOperationComplete(ctx, "host-power-off", machineName)
	h.waitForPowerState(ctx, allocated, metalredfish.PowerOff)

	h.createMachineOperation(ctx, "host-power-on", machinav1alpha3.OperationHostPowerOn, nil)
	h.waitMachineOperationComplete(ctx, "host-power-on", machineName)
	h.waitForPowerState(ctx, allocated, metalredfish.PowerOn)
	h.waitNodeReady(ctx, machineName)
	bootID = h.waitNodeBootIDChanged(ctx, machineName, bootID)

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{machinav1alpha3.MachineSiteLabelKey: siteName}}
	h.createMachineOperation(ctx, "host-reboot", machinav1alpha3.OperationHostReboot, selector)
	h.waitMachineOperationComplete(ctx, "host-reboot", machineName)
	h.waitForPowerState(ctx, allocated, metalredfish.PowerOn)
	h.waitNodeReady(ctx, machineName)
	h.waitNodeBootIDChanged(ctx, machineName, bootID)

	if err := terminateProcess(ctx, agentCmd); err != nil {
		t.Logf("stop agent HTTP helper: %v", err)
	}

	if err := terminateProcess(ctx, metalmanCmd); err != nil {
		t.Logf("stop metalman: %v", err)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	repoRoot := filepath.Dir(filepath.Dir(wd))
	artifacts := filepath.Join(wd, ".artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatalf("create artifact directory: %v", err)
	}
	private, err := os.MkdirTemp("", "metalman-smoke-*")
	if err != nil {
		t.Fatalf("create private directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(private) })

	images := smokeImages{
		host:    strings.TrimSpace(os.Getenv("METALMAN_SMOKE_HOST_IMAGE")),
		netboot: strings.TrimSpace(os.Getenv("METALMAN_SMOKE_NETBOOT_IMAGE")),
		agent:   strings.TrimSpace(os.Getenv("METALMAN_SMOKE_AGENT_IMAGE")),
	}

	var missing []string
	if images.host == "" {
		missing = append(missing, "METALMAN_SMOKE_HOST_IMAGE")
	}
	if images.netboot == "" {
		missing = append(missing, "METALMAN_SMOKE_NETBOOT_IMAGE")
	}
	if images.agent == "" {
		missing = append(missing, "METALMAN_SMOKE_AGENT_IMAGE")
	}
	if len(missing) > 0 {
		t.Fatalf("required image environment variables are not set: %s", strings.Join(missing, ", "))
	}

	return &harness{t: t, repoRoot: repoRoot, artifacts: artifacts, private: private, images: images}
}

func loadDefaultRESTConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if contextName := strings.TrimSpace(os.Getenv("METALMAN_SMOKE_PLAYPEN_CONTEXT")); contextName != "" {
		overrides.CurrentContext = contextName
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	return config.ClientConfig()
}

func (h *harness) checkPrereqs(ctx context.Context) {
	h.t.Helper()

	for _, bin := range []string{"bridge", "curl", "go", "gzip", "ip", "iptables", "sysctl", "tar", "wg"} {
		if _, err := exec.LookPath(bin); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}

	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("sudo"); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", "sudo")
		}

		cmd := exec.CommandContext(ctx, "sudo", "-n", "true")
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Skipf("passwordless sudo unavailable (%v): %s", err, strings.TrimSpace(string(out)))
		}
	}
}

func (h *harness) buildBinaries(ctx context.Context) {
	h.t.Helper()
	h.run(ctx, h.repoRoot, nil, "go", "build", "-ldflags", "-X github.com/Azure/unbounded/internal/metalman/commands.DefaultNetbootImage="+h.images.netboot, "-o", filepath.Join(h.repoRoot, "bin", "metalman"), "./cmd/metalman/main.go")
	h.run(ctx, h.repoRoot, []string{"GOOS=linux", "GOARCH=amd64"}, "go", "build", "-o", filepath.Join(h.repoRoot, "bin", "unbounded-agent"), "./cmd/agent/main.go")
}

func (h *harness) packageAgent(ctx context.Context) {
	h.t.Helper()

	archivePath := filepath.Join(h.artifacts, agentArchiveName)
	buildDir := filepath.Join(h.artifacts, "agent-package")
	if err := os.RemoveAll(buildDir); err != nil {
		h.t.Fatalf("clean agent package dir: %v", err)
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		h.t.Fatalf("create agent package dir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(h.repoRoot, "bin", "unbounded-agent"))
	if err != nil {
		h.t.Fatalf("read unbounded-agent binary: %v", err)
	}

	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	if err := tw.WriteHeader(&tar.Header{Name: "unbounded-agent", Mode: 0o755, Size: int64(len(data))}); err != nil {
		h.t.Fatalf("write agent tar header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		h.t.Fatalf("write agent tar payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		h.t.Fatalf("close agent tar: %v", err)
	}

	cmd := exec.CommandContext(ctx, "gzip", "-c")
	cmd.Stdin = bytes.NewReader(tarbuf.Bytes())
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("gzip agent archive: %v", err)
	}

	if err := os.WriteFile(archivePath, out, 0o644); err != nil {
		h.t.Fatalf("write agent archive: %v", err)
	}
}

func (h *harness) configureTargetControlPlane(ctx context.Context, ppClient *playpenclient.Client, playpenREST *rest.Config) targetControlPlane {
	h.t.Helper()

	cp, err := ppClient.AllocateControlPlane(ctx, playpenclient.AllocateOptions{})
	if err == nil {
		h.cleanupControlPlane(cp)
		if err := h.writeChildKubeconfig(cp.Kubeconfig()); err != nil {
			h.t.Fatalf("write child kubeconfig: %v", err)
		}

		return targetControlPlane{guestAPIServerURL: cp.Metadata.ControlPlane.GuestAPIServerURL}
	}

	h.t.Logf("Playpen control-plane allocation unavailable; using configured cluster as target control plane: %v", err)
	if err := h.writeFallbackKubeconfig(playpenREST); err != nil {
		h.t.Fatalf("write fallback kubeconfig: %v", err)
	}

	return targetControlPlane{guestAPIServerURL: h.fallbackGuestAPIServerURL(playpenREST)}
}

func (h *harness) writeFallbackKubeconfig(cfg *rest.Config) error {
	cfg = rest.CopyConfig(cfg)
	if cfg == nil || strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("default kubeconfig has no server URL")
	}

	const (
		clusterName = "target"
		userName    = "target"
		contextName = "target"
	)

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.ClientCertificateData = cfg.CertData
	authInfo.ClientKeyData = cfg.KeyData
	authInfo.Token = cfg.BearerToken
	authInfo.TokenFile = cfg.BearerTokenFile
	authInfo.Username = cfg.Username
	authInfo.Password = cfg.Password
	authInfo.AuthProvider = cfg.AuthProvider
	authInfo.Exec = cfg.ExecProvider

	cluster := clientcmdapi.NewCluster()
	cluster.Server = cfg.Host
	cluster.CertificateAuthorityData = cfg.CAData
	if len(cluster.CertificateAuthorityData) == 0 && cfg.CAFile != "" {
		caData, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return fmt.Errorf("read fallback kubeconfig CA file: %w", err)
		}
		cluster.CertificateAuthorityData = caData
	}
	cluster.InsecureSkipTLSVerify = cfg.Insecure
	cluster.TLSServerName = cfg.ServerName

	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters[clusterName] = cluster
	kubeconfig.AuthInfos[userName] = authInfo
	kubeconfig.Contexts[contextName] = &clientcmdapi.Context{Cluster: clusterName, AuthInfo: userName}
	kubeconfig.CurrentContext = contextName

	data, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return fmt.Errorf("encode fallback kubeconfig: %w", err)
	}

	h.childKube = filepath.Join(h.private, "target.kubeconfig")
	return os.WriteFile(h.childKube, data, 0o600)
}

func (h *harness) fallbackGuestAPIServerURL(cfg *rest.Config) string {
	h.t.Helper()

	if override := strings.TrimSpace(os.Getenv("METALMAN_SMOKE_TARGET_APISERVER_URL")); override != "" {
		return override
	}

	server := strings.TrimSpace(cfg.Host)
	if server == "" {
		h.t.Fatal("default kubeconfig has no server URL")
	}

	return server
}

func (h *harness) writeChildKubeconfig(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("playpen control plane returned an empty kubeconfig")
	}

	h.childKube = filepath.Join(h.private, "child.kubeconfig")
	return os.WriteFile(h.childKube, []byte(raw+"\n"), 0o600)
}

func (h *harness) initChildClients() error {
	cfg, err := clientcmd.BuildConfigFromFlags("", h.childKube)
	if err != nil {
		return fmt.Errorf("load child kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := machinav1alpha3.AddToScheme(scheme); err != nil {
		return err
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create controller-runtime client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	h.childREST = cfg
	h.childClient = k8sClient
	h.childset = clientset

	return nil
}

func (h *harness) ensureClusterInfo(ctx context.Context) error {
	cfg, err := clientcmd.LoadFromFile(h.childKube)
	if err != nil {
		return fmt.Errorf("load target kubeconfig: %w", err)
	}

	if len(cfg.Clusters) == 0 {
		return fmt.Errorf("target kubeconfig has no clusters")
	}

	for _, cluster := range cfg.Clusters {
		if strings.TrimSpace(cluster.Server) == "" {
			return fmt.Errorf("target kubeconfig cluster has no server URL")
		}
		caData := cluster.CertificateAuthorityData
		if len(caData) == 0 && cluster.CertificateAuthority != "" {
			caData, err = os.ReadFile(cluster.CertificateAuthority)
			if err != nil {
				return fmt.Errorf("read target kubeconfig CA file: %w", err)
			}
		}
		if len(caData) == 0 {
			return fmt.Errorf("target kubeconfig cluster has no certificate authority data")
		}

		publicNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespacePublic}}
		if err := h.childClient.Create(ctx, publicNS); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create kube-public namespace: %w", err)
		}

		clusterInfo := clientcmdapi.NewConfig()
		clusterInfo.Clusters["default"] = &clientcmdapi.Cluster{
			Server:                   cluster.Server,
			CertificateAuthorityData: caData,
		}
		clusterInfo.Contexts["default"] = &clientcmdapi.Context{Cluster: "default"}
		clusterInfo.CurrentContext = "default"

		data, err := clientcmd.Write(*clusterInfo)
		if err != nil {
			return fmt.Errorf("encode cluster-info kubeconfig: %w", err)
		}

		cm := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: metav1.NamespacePublic},
			Data:       map[string]string{"kubeconfig": string(data)},
		}
		if err := h.childClient.Patch(ctx, cm, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply cluster-info ConfigMap: %w", err)
		}

		return nil
	}

	return fmt.Errorf("target kubeconfig has no usable clusters")
}

func (h *harness) applyChildManifests(ctx context.Context) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, path := range []string{
		filepath.Join(h.repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_machines.yaml"),
		filepath.Join(h.repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_machineoperations.yaml"),
		filepath.Join(h.repoRoot, "deploy", "machina", "rendered", "01-namespace.yaml"),
		filepath.Join(h.repoRoot, "deploy", "machina", "rendered", "06-metalman-rbac.yaml"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", path, err)
		}
		if err := internalkube.ApplyManifests(ctx, logger, h.childClient, fieldManager, data); err != nil {
			return fmt.Errorf("apply manifest %s: %w", path, err)
		}
	}

	return h.waitForCRDs(ctx)
}

func (h *harness) waitForCRDs(ctx context.Context) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := h.childset.Discovery().ServerResourcesForGroupVersion(machinav1alpha3.GroupVersion.String())
		if err == nil {
			return true, nil
		}

		h.t.Logf("waiting for Machina CRDs to register: %v", err)

		return false, nil
	})
}

func (h *harness) configurePlaypenTunnel(ctx context.Context, playpenSet kubernetes.Interface, allocated *playpenclient.Playpen) {
	h.t.Helper()

	if err := h.configureAndVerifyPlaypenTunnel(ctx, allocated); err == nil {
		return
	} else {
		h.t.Logf("published Playpen endpoint %s:%d did not become reachable: %v", allocated.Metadata.Endpoint.Host, allocated.Metadata.Endpoint.WireGuardUDPPort, err)
	}

	internalIP, err := h.runnerNodeInternalIP(ctx, playpenSet, allocated)
	if err != nil {
		h.dumpTunnelDiagnostics(ctx, allocated)
		h.t.Fatalf("resolve Playpen runner internal endpoint: %v", err)
	}
	if internalIP == allocated.Metadata.Endpoint.Host {
		h.dumpTunnelDiagnostics(ctx, allocated)
		h.t.Fatalf("published Playpen endpoint %s is already the runner node internal IP", internalIP)
	}

	h.t.Logf("retrying Playpen tunnel through runner node internal IP %s", internalIP)
	allocated.OverrideEndpoint(internalIP, allocated.Metadata.Endpoint.WireGuardUDPPort)
	if err := h.configureAndVerifyPlaypenTunnel(ctx, allocated); err != nil {
		h.dumpTunnelDiagnostics(ctx, allocated)
		h.t.Fatalf("verify Playpen tunnel connectivity through internal endpoint %s: %v", internalIP, err)
	}
}

func (h *harness) configureAndVerifyPlaypenTunnel(ctx context.Context, allocated *playpenclient.Playpen) error {
	if err := allocated.ConfigureTunnel(ctx); err != nil {
		return fmt.Errorf("configure tunnel: %w", err)
	}
	if err := h.configureNamespaceDNS(ctx, allocated); err != nil {
		return fmt.Errorf("configure namespace DNS: %w", err)
	}

	return h.verifyTunnelConnectivity(ctx, allocated)
}

func (h *harness) runnerNodeInternalIP(ctx context.Context, playpenSet kubernetes.Interface, allocated *playpenclient.Playpen) (string, error) {
	pod, err := playpenSet.CoreV1().Pods(allocated.Metadata.Pod.Namespace).Get(ctx, allocated.Metadata.Pod.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get runner pod: %w", err)
	}
	if pod.Spec.NodeName == "" {
		return "", fmt.Errorf("runner pod has no nodeName")
	}

	node, err := playpenSet.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get runner node %s: %w", pod.Spec.NodeName, err)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && strings.TrimSpace(addr.Address) != "" {
			return addr.Address, nil
		}
	}

	return "", fmt.Errorf("runner node %s has no InternalIP", pod.Spec.NodeName)
}

func (h *harness) configureNamespaceDNS(ctx context.Context, allocated *playpenclient.Playpen) error {
	nameservers := hostNameservers()
	if len(nameservers) == 0 {
		nameservers = allocated.Metadata.Network.DNS
	}
	if len(nameservers) == 0 {
		return fmt.Errorf("no non-loopback DNS nameservers found")
	}

	var data strings.Builder
	for _, nameserver := range nameservers {
		data.WriteString("nameserver ")
		data.WriteString(nameserver)
		data.WriteByte('\n')
	}

	nsDir := filepath.Join("/etc", "netns", allocated.TunnelConfig().NetworkNamespace)
	if os.Geteuid() == 0 {
		if err := os.MkdirAll(nsDir, 0o755); err != nil {
			return fmt.Errorf("create namespace resolv.conf directory: %w", err)
		}

		return os.WriteFile(filepath.Join(nsDir, "resolv.conf"), []byte(data.String()), 0o644)
	}

	if out, err := exec.CommandContext(ctx, "sudo", "-n", "mkdir", "-p", nsDir).CombinedOutput(); err != nil {
		return fmt.Errorf("create namespace resolv.conf directory: %w: %s", err, strings.TrimSpace(string(out)))
	}

	cmd := exec.CommandContext(ctx, "sudo", "-n", "tee", filepath.Join(nsDir, "resolv.conf"))
	cmd.Stdin = strings.NewReader(data.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write namespace resolv.conf: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func (h *harness) verifyTunnelConnectivity(ctx context.Context, allocated *playpenclient.Playpen) error {
	h.t.Helper()

	readyURL := strings.TrimRight(allocated.Metadata.Redfish["url"], "/") + "/readyz"
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		cmd, err := allocated.Command(ctx, "curl", "-fsSk", readyURL)
		if err != nil {
			return false, err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Logf("waiting for Playpen Redfish ready: %v: %s", err, strings.TrimSpace(string(out)))

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf("verify Playpen tunnel connectivity to %s: %w", readyURL, err)
	}

	return nil
}

func (h *harness) dumpTunnelDiagnostics(ctx context.Context, allocated *playpenclient.Playpen) {
	cfg := allocated.TunnelConfig()
	for _, args := range [][]string{
		{"addr", "show"},
		{"route"},
		{"route", "get", "10.88.0.1"},
		{"-d", "link", "show", "dev", cfg.VXLANInterface},
	} {
		cmd, err := allocated.Command(ctx, "ip", args...)
		if err != nil {
			h.t.Logf("create ip diagnostic command %s: %v", strings.Join(args, " "), err)
			continue
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.t.Logf("ip %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			continue
		}
		h.t.Logf("ip %s\n%s", strings.Join(args, " "), string(out))
	}

	for _, args := range [][]string{
		{"show", cfg.WireGuardInterface},
		{"show", cfg.WireGuardInterface, "endpoints"},
		{"show", cfg.WireGuardInterface, "latest-handshakes"},
	} {
		cmd, err := allocated.Command(ctx, "wg", args...)
		if err != nil {
			h.t.Logf("create wg diagnostic command %s: %v", strings.Join(args, " "), err)
			continue
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.t.Logf("wg %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			continue
		}
		h.t.Logf("wg %s\n%s", strings.Join(args, " "), string(out))
	}

	for _, args := range [][]string{
		{"get", "pod", allocated.Metadata.Pod.Name, "-n", allocated.Metadata.Pod.Namespace, "-o", "wide"},
		{"logs", "-n", allocated.Metadata.Pod.Namespace, allocated.Metadata.Pod.Name, "--tail=200"},
	} {
		if contextName := strings.TrimSpace(os.Getenv("METALMAN_SMOKE_PLAYPEN_CONTEXT")); contextName != "" {
			args = append([]string{"--context", contextName}, args...)
		}

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.t.Logf("kubectl %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			continue
		}
		h.t.Logf("kubectl %s\n%s", strings.Join(args, " "), string(out))
	}
}

func hostNameservers() []string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var nameservers []string
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "nameserver" || strings.HasPrefix(fields[1], "127.") || fields[1] == "::1" {
				continue
			}
			nameservers = append(nameservers, fields[1])
		}
		if len(nameservers) > 0 {
			return nameservers
		}
	}

	return nil
}

func (h *harness) startSerialLog(ctx context.Context, allocated *playpenclient.Playpen) {
	h.t.Helper()

	path := filepath.Join(h.artifacts, "serial-console.log")
	file, err := os.Create(path)
	if err != nil {
		h.t.Fatalf("create serial log: %v", err)
	}
	h.t.Cleanup(func() { file.Close() })

	go func() {
		if err := <-allocated.StreamConsoleLogs(ctx, file); err != nil && ctx.Err() == nil {
			h.t.Logf("stream serial logs: %v", err)
		}
	}()
}

func (h *harness) startAgentHTTPServer(ctx context.Context, allocated *playpenclient.Playpen) *exec.Cmd {
	h.t.Helper()

	cmd, err := allocated.Command(ctx,
		"env",
		"METALMAN_SMOKE_AGENT_HELPER=1",
		"METALMAN_SMOKE_AGENT_DIR="+h.artifacts,
		"METALMAN_SMOKE_AGENT_ADDR="+allocated.Metadata.Network.GatewayIPv4+":"+agentHTTPPort,
		os.Args[0], "-test.run", "^TestMetalmanSmokeOnPlaypen$", "-test.v",
	)
	if err != nil {
		h.t.Fatalf("create agent HTTP helper command: %v", err)
	}
	cmd.Env = os.Environ()
	h.attachLog(cmd, "agent-http.log")
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start agent HTTP helper: %v", err)
	}
	h.t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := terminateProcess(stopCtx, cmd); err != nil {
			h.t.Logf("cleanup agent HTTP helper: %v", err)
		}
	})

	return cmd
}

func runAgentHTTPHelper(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("METALMAN_SMOKE_AGENT_DIR"))
	addr := strings.TrimSpace(os.Getenv("METALMAN_SMOKE_AGENT_ADDR"))
	if dir == "" || addr == "" {
		t.Fatalf("agent helper requires METALMAN_SMOKE_AGENT_DIR and METALMAN_SMOKE_AGENT_ADDR")
	}

	srv := &http.Server{Addr: addr, Handler: http.FileServer(http.Dir(dir))}
	t.Logf("serving agent artifacts from %s on %s", dir, addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("agent HTTP server: %v", err)
	}
}

func (h *harness) startMetalman(ctx context.Context, allocated *playpenclient.Playpen, guestAPIServerURL string) *exec.Cmd {
	h.t.Helper()

	cacheDir := filepath.Join(h.artifacts, "metalman-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		h.t.Fatalf("create metalman cache: %v", err)
	}

	gateway := allocated.Metadata.Network.GatewayIPv4
	cmd, err := allocated.Command(ctx,
		"env",
		"KUBECONFIG="+h.childKube,
		"METALMAN_APISERVER_URL="+guestAPIServerURL,
		filepath.Join(h.repoRoot, "bin", "metalman"),
		"serve-pxe",
		"--site="+siteName,
		"--bind-address="+gateway,
		"--cache-dir="+cacheDir,
		"--serve-url=http://"+gateway+":"+metalmanHTTPPort,
		"--http-port="+metalmanHTTPPort,
		"--health-port="+metalmanHealthPort,
		"--dhcp-interface="+allocated.TunnelConfig().VXLANInterface,
		"--default-netboot-image="+h.images.netboot,
		"--leader-elect-lease-duration=15s",
		"--leader-elect-renew-deadline=10s",
		"--leader-elect-retry-period=2s",
		"--operation-poll-interval=5s",
	)
	if err != nil {
		h.t.Fatalf("create metalman command: %v", err)
	}
	cmd.Env = os.Environ()
	h.attachLog(cmd, "metalman.log")
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start metalman: %v", err)
	}
	h.t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := terminateProcess(stopCtx, cmd); err != nil {
			h.t.Logf("cleanup metalman: %v", err)
		}
	})

	return cmd
}

func (h *harness) attachLog(cmd *exec.Cmd, name string) {
	h.t.Helper()

	path := filepath.Join(h.artifacts, name)
	file, err := os.Create(path)
	if err != nil {
		h.t.Fatalf("create log %s: %v", name, err)
	}
	h.t.Cleanup(func() { file.Close() })
	cmd.Stdout = file
	cmd.Stderr = file
}

func (h *harness) waitForMetalmanReady(ctx context.Context, allocated *playpenclient.Playpen) {
	h.t.Helper()

	url := "http://127.0.0.1:" + metalmanHealthPort + "/readyz"
	err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		cmd, err := allocated.Command(ctx, "curl", "-fsS", url)
		if err != nil {
			return false, err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Logf("waiting for metalman ready: %v: %s", err, strings.TrimSpace(string(out)))

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for metalman ready: %v", err)
	}
}

func (h *harness) cleanupChildResources(ctx context.Context) {
	h.t.Helper()

	for _, name := range []string{"host-replace", "host-power-off", "host-power-on", "host-reboot"} {
		_ = h.childClient.Delete(ctx, &machinav1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	_ = h.childClient.Delete(ctx, &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName}})
	_ = h.childClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machineName}})
	_ = h.childClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: redfishSecretName, Namespace: metav1.NamespaceDefault}})
}

func (h *harness) createRedfishSecret(ctx context.Context, allocated *playpenclient.Playpen) {
	h.t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: redfishSecretName, Namespace: metav1.NamespaceDefault},
		StringData: map[string]string{redfishSecretKey: allocated.Metadata.Redfish["password"]},
	}
	if err := h.childClient.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create Redfish password secret: %v", err)
	}
}

func (h *harness) createMachine(ctx context.Context, allocated *playpenclient.Playpen, agentURL string) {
	h.t.Helper()

	machine := &machinav1alpha3.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: machinav1alpha3.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: machineName,
			Labels: map[string]string{
				machinav1alpha3.MachineSiteLabelKey: siteName,
			},
		},
		Spec: machinav1alpha3.MachineSpec{
			PXE: &machinav1alpha3.PXESpec{
				Image:        h.images.host,
				Architecture: machinav1alpha3.PXEArchitectureAMD64,
				NetbootImage: h.images.netboot,
				DHCPLeases: []machinav1alpha3.DHCPLease{{
					MAC:        allocated.Metadata.Network.GuestMAC,
					IPv4:       allocated.Metadata.Network.GuestIPv4,
					SubnetMask: allocated.Metadata.Network.SubnetMask,
					Gateway:    allocated.Metadata.Network.GatewayIPv4,
					DNS:        allocated.Metadata.Network.DNS,
				}},
				Redfish: &machinav1alpha3.RedfishSpec{
					URL:      allocated.Metadata.Redfish["url"],
					Username: allocated.Metadata.Redfish["username"],
					DeviceID: allocated.Metadata.Redfish["deviceID"],
					PasswordRef: machinav1alpha3.SecretKeySelector{
						Name:      redfishSecretName,
						Namespace: metav1.NamespaceDefault,
						Key:       redfishSecretKey,
					},
				},
			},
			Kubernetes: &machinav1alpha3.KubernetesSpec{
				NodeLabels: map[string]string{nodeSmokeLabelKey: nodeSmokeLabelValue},
			},
			Agent: &machinav1alpha3.AgentSpec{Image: h.images.agent, URL: agentURL},
		},
	}

	if err := h.childClient.Create(ctx, machine); err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("create Machine: %v", err)
	}
}

func (h *harness) createMachineOperation(ctx context.Context, name string, op machinav1alpha3.OperationKind, selector *metav1.LabelSelector) {
	h.t.Helper()

	ttl := int32(3600)
	mop := &machinav1alpha3.MachineOperation{
		TypeMeta: metav1.TypeMeta{APIVersion: machinav1alpha3.GroupVersion.String(), Kind: "MachineOperation"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: machinav1alpha3.MachineOperationSpec{
			OperationKind:           op,
			MachineSelector:         selector,
			TTLSecondsAfterFinished: &ttl,
		},
	}
	if selector == nil {
		mop.Spec.MachineRef = machineName
	}

	if err := h.childClient.Create(ctx, mop); err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("create MachineOperation %s: %v", name, err)
	}
}

func (h *harness) waitMachineOperationComplete(ctx context.Context, name, wantMachine string) {
	h.t.Helper()

	key := types.NamespacedName{Name: name}
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 25*time.Minute, true, func(ctx context.Context) (bool, error) {
		mop := &machinav1alpha3.MachineOperation{}
		if err := h.childClient.Get(ctx, key, mop); err != nil {
			return false, err
		}

		h.t.Logf("operation %s phase=%s message=%q targets=%d", name, mop.Status.Phase, mop.Status.Message, len(mop.Status.Targets))
		if mop.Status.Phase == machinav1alpha3.OperationPhaseFailed {
			return false, fmt.Errorf("operation %s failed: %s", name, mop.Status.Message)
		}
		if mop.Status.Phase != machinav1alpha3.OperationPhaseComplete {
			return false, nil
		}
		if len(mop.Status.Targets) == 0 {
			return false, nil
		}
		for _, target := range mop.Status.Targets {
			if target.MachineRef == wantMachine && target.Phase == machinav1alpha3.OperationPhaseComplete {
				return true, nil
			}
		}

		return false, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for MachineOperation %s complete: %v", name, err)
	}
}

func (h *harness) waitMachineCondition(ctx context.Context, conditionType string, status metav1.ConditionStatus, reason string) {
	h.t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 20*time.Minute, true, func(ctx context.Context) (bool, error) {
		machine := &machinav1alpha3.Machine{}
		if err := h.childClient.Get(ctx, types.NamespacedName{Name: machineName}, machine); err != nil {
			return false, err
		}
		for _, condition := range machine.Status.Conditions {
			if condition.Type == conditionType {
				h.t.Logf("machine condition %s=%s reason=%s message=%q", condition.Type, condition.Status, condition.Reason, condition.Message)
				return condition.Status == status && (reason == "" || condition.Reason == reason), nil
			}
		}

		return false, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for Machine condition %s: %v", conditionType, err)
	}
}

func (h *harness) waitNodeReady(ctx context.Context, name string) {
	h.t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		node, err := h.childset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				h.t.Logf("node %s Ready=%s reason=%s", name, condition.Status, condition.Reason)
				return condition.Status == corev1.ConditionTrue, nil
			}
		}

		return false, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for node %s Ready: %v", name, err)
	}
}

func (h *harness) assertNodeLabel(ctx context.Context, name, key, want string) {
	h.t.Helper()

	node, err := h.childset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("get node %s: %v", name, err)
	}
	if got := node.Labels[key]; got != want {
		h.t.Fatalf("node %s label %s = %q, want %q", name, key, got, want)
	}
}

func (h *harness) waitNodeBootID(ctx context.Context, name string) string {
	h.t.Helper()

	var bootID string
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		node, err := h.childset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		bootID = node.Status.NodeInfo.BootID

		return strings.TrimSpace(bootID) != "", nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for node %s boot ID: %v", name, err)
	}

	return bootID
}

func (h *harness) waitNodeBootIDChanged(ctx context.Context, name, previous string) string {
	h.t.Helper()

	var bootID string
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		node, err := h.childset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		bootID = node.Status.NodeInfo.BootID

		return bootID != "" && bootID != previous, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for node %s boot ID to change from %s: %v", name, previous, err)
	}

	return bootID
}

func (h *harness) waitForPowerState(ctx context.Context, allocated *playpenclient.Playpen, want metalredfish.PowerState) {
	h.t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		state, err := h.redfishPowerState(ctx, allocated)
		if err != nil {
			h.t.Logf("waiting for Redfish power state %s: %v", want, err)

			return false, nil
		}
		h.t.Logf("Redfish power state=%s", state)

		return state == want, nil
	})
	if err != nil {
		h.collectDiagnostics(ctx)
		h.t.Fatalf("wait for Redfish power state %s: %v", want, err)
	}
}

func (h *harness) redfishPowerState(ctx context.Context, allocated *playpenclient.Playpen) (metalredfish.PowerState, error) {
	url := strings.TrimRight(allocated.Metadata.Redfish["url"], "/") + "/redfish/v1/Systems/" + allocated.Metadata.Redfish["deviceID"]
	cmd, err := allocated.Command(ctx,
		"curl",
		"-fsSk",
		"-u", allocated.Metadata.Redfish["username"]+":"+allocated.Metadata.Redfish["password"],
		url,
	)
	if err != nil {
		return "", err
	}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("query Redfish power state: %w", err)
	}

	var system struct {
		PowerState metalredfish.PowerState `json:"PowerState"`
	}
	if err := json.Unmarshal(out, &system); err != nil {
		return "", fmt.Errorf("decode Redfish system response: %w", err)
	}
	if system.PowerState == "" {
		return "", fmt.Errorf("Redfish system response has no PowerState")
	}

	return system.PowerState, nil
}

func (h *harness) collectDiagnostics(ctx context.Context) {
	if h.childClient == nil || h.childset == nil {
		return
	}

	writeObject := func(name string, obj client.Object) {
		if err := h.childClient.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, obj); err != nil {
			_ = os.WriteFile(filepath.Join(h.artifacts, name), []byte(err.Error()+"\n"), 0o644)

			return
		}
		data, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			data = []byte(fmt.Sprintf("%#v\n", obj))
		}
		_ = os.WriteFile(filepath.Join(h.artifacts, name), data, 0o644)
	}

	writeObject("machine.txt", &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName}})
	for _, name := range []string{"host-replace", "host-power-off", "host-power-on", "host-reboot"} {
		writeObject("machineoperation-"+name+".txt", &machinav1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}

	if node, err := h.childset.CoreV1().Nodes().Get(ctx, machineName, metav1.GetOptions{}); err == nil {
		_ = os.WriteFile(filepath.Join(h.artifacts, "node.txt"), []byte(fmt.Sprintf("%#v\n", node)), 0o644)
	}
	if events, err := h.childset.CoreV1().Events("").List(ctx, metav1.ListOptions{}); err == nil {
		_ = os.WriteFile(filepath.Join(h.artifacts, "events.txt"), []byte(fmt.Sprintf("%#v\n", events)), 0o644)
	}
}

func (h *harness) cleanupPlaypen(allocated *playpenclient.Playpen) {
	h.t.Helper()
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := allocated.Close(ctx); err != nil {
			h.t.Logf("close playpen runner: %v", err)
		}
	})
}

func (h *harness) cleanupControlPlane(cp *playpenclient.ControlPlane) {
	h.t.Helper()
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cp.Close(ctx); err != nil {
			h.t.Logf("close playpen control plane: %v", err)
		}
	})
}

func (h *harness) run(ctx context.Context, dir string, env []string, name string, args ...string) {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("run %s %s: %v", name, strings.Join(args, " "), err)
	}
}

func terminateProcess(ctx context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return nil
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done

		return ctx.Err()
	case err := <-done:
		if err == nil || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}

		return err
	}
}
