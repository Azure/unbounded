// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build e2e

package metalman

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	clusterIP       = "192.168.200.2"
	guestIP         = "192.168.200.10"
	bridgeIP        = "192.168.200.1"
	clientCIDR      = "172.30.11.2/30"
	clientGateway   = "172.30.11.1"
	clientNamespace = "metalman-smoke"
	playpenNS       = "metalman-smoke"
	machineName     = "smoke-node"
	siteName        = "smoke"
	vxlanVNI        = "201"
	sandboxImage    = "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9"
)

type harness struct {
	t          *testing.T
	root       string
	cluster    string
	node       string
	kubeconfig string
	privateDir string
	artifacts  string
	client     *exec.Cmd
	podIP      string
	nodeIP     string
	registry   bool
	resolver   string
}

type helperState struct {
	Pod string `json:"pod"`
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

func TestMetalmanSmokeOnPlaypen(t *testing.T) {
	if os.Getenv("METALMAN_SMOKE_HELPER") == "1" {
		if err := runHelper(); err != nil {
			t.Fatalf("helper: %v", err)
		}

		return
	}

	if os.Geteuid() != 0 {
		t.Fatal("metalman smoke test must run as root (use sudo -E make e2e-metalman)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	h := newHarness(t)
	defer h.cleanup()
	defer func() {
		if t.Failed() {
			h.collectDiagnostics()
		}
	}()

	h.requireCommands("docker", "kind", "kubectl", "make", "ip", "iptables", "nsenter")
	t.Log("preparing binaries, images, manifests, and the Playpen pod")
	h.prepare(ctx)
	t.Log("starting the Playpen client network")
	state := h.startPlaypenClient(ctx)
	h.startDNSRelay(ctx)
	h.startArtifactProxy(ctx)
	t.Log("Playpen client network is ready; configuring the kind control plane")
	h.configureControlPlane(ctx)
	h.signalControlPlaneReady()
	h.waitFile(ctx, "metalman-ready", 2*time.Minute)
	t.Log("Metalman is ready; creating the Machine")
	h.createMachine(ctx, state)
	t.Log("starting HostReplace and waiting for provisioning")
	h.runReplace(ctx)
	t.Log("cloud-init completed; waiting for the worker Node")
	h.assertNode(ctx)
	t.Log("worker Node is Ready; running power operations")
	h.runPowerOperations(ctx)
	t.Log("Metalman smoke test completed")
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	cluster := os.Getenv("METALMAN_SMOKE_CLUSTER")
	if cluster == "" {
		cluster = "kind"
	}

	artifacts := filepath.Join(root, "e2e", "metalman", ".artifacts")
	privateDir := filepath.Join(root, "tmp", "metalman-smoke")
	if err := os.RemoveAll(artifacts); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(privateDir); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{artifacts, privateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return &harness{
		t:          t,
		root:       root,
		cluster:    cluster,
		node:       cluster + "-control-plane",
		kubeconfig: filepath.Join(privateDir, "kubeconfig"),
		privateDir: privateDir,
		artifacts:  artifacts,
	}
}

func (h *harness) requireCommands(names ...string) {
	h.t.Helper()

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			h.t.Fatalf("required command %s: %v", name, err)
		}
	}

	if info, err := os.Stat("/dev/kvm"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		h.t.Fatal("/dev/kvm character device is required")
	}
}

func (h *harness) prepare(ctx context.Context) {
	h.t.Helper()
	h.resolver = hostResolver(h.t)
	h.t.Logf("using host DNS resolver %s for the guest", h.resolver)

	kubeconfig := h.output(ctx, "kind", "get", "kubeconfig", "--name", h.cluster)
	h.writeFile(h.kubeconfig, []byte(kubeconfig), 0o600)

	h.run(ctx, "make", "machina-manifests", "net-manifests")
	h.run(ctx, "go", "build", "-o", "bin/playpen", "./cmd/playpen")
	h.run(ctx, "go", "build", "-o", "bin/metalman", "./cmd/metalman")
	h.run(ctx, "go", "build", "-o", "bin/kubectl-unbounded", "./cmd/kubectl-unbounded")
	h.runEnv(ctx, []string{"GOOS=linux", "GOARCH=amd64"}, "go", "build", "-o", "bin/unbounded-agent", "./cmd/agent")
	h.packageAgent()

	for _, image := range []string{
		"localhost:5555/unbounded/host-ubuntu2404:smoke",
		"localhost:5555/unbounded/netboot:smoke",
		"localhost:5555/unbounded/agent-ubuntu2404:smoke",
	} {
		h.run(ctx, "docker", "image", "inspect", image)
	}

	var version struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal([]byte(h.kubectlOutput(ctx, "version", "-o", "json")), &version); err != nil {
		h.t.Fatalf("parse Kubernetes version: %v", err)
	}
	if version.ServerVersion.GitVersion == "" {
		h.t.Fatal("Kubernetes server version is empty")
	}
	kubeProxyImage := "registry.k8s.io/kube-proxy:" + version.ServerVersion.GitVersion
	h.kubectl(ctx, "-n", "kube-system", "set", "image", "daemonset/kube-proxy", "kube-proxy="+kubeProxyImage)
	kindnetImage := strings.TrimSpace(h.kubectlOutput(ctx, "-n", "kube-system", "get", "daemonset", "kindnet", "-o", "jsonpath={.spec.template.spec.containers[?(@.name=='kindnet-cni')].image}"))
	if kindnetImage == "" {
		h.t.Fatal("kindnet image is empty")
	}
	h.run(ctx, "docker", "pull", sandboxImage)
	h.run(ctx, "docker", "pull", kubeProxyImage)
	h.run(ctx, "docker", "pull", kindnetImage)
	h.run(ctx, "docker", "save", "-o", filepath.Join(h.privateDir, "node-images.tar"), sandboxImage, kubeProxyImage, kindnetImage)
	h.writeSHA256(filepath.Join(h.privateDir, "node-images.tar"))

	if !urlReady("http://127.0.0.1:5555/v2/") {
		h.runIgnore("docker", "rm", "-f", "unbounded-metalman-smoke-registry")
		h.run(ctx, "docker", "run", "-d", "--name", "unbounded-metalman-smoke-registry", "-p", "5555:5000", "registry:2")
		h.registry = true
		h.waitHTTP(ctx, "http://127.0.0.1:5555/v2/", time.Minute)
	}

	for _, image := range []string{"host-ubuntu2404", "netboot", "agent-ubuntu2404"} {
		h.run(ctx, "docker", "push", "localhost:5555/unbounded/"+image+":smoke")
	}

	h.run(ctx, "docker", "build", "-t", "playpen:metalman-smoke", "-f", "images/playpen/Containerfile", ".")
	h.run(ctx, "kind", "load", "docker-image", "--name", h.cluster, "playpen:metalman-smoke")
	h.runEnv(ctx, []string{
		"PLAYPEN_NAMESPACE=" + playpenNS,
		"PLAYPEN_IMAGE=playpen:metalman-smoke",
		"PLAYPEN_VXLAN_REMOTE=172.30.11.2",
		"PLAYPEN_VXLAN_VNI=" + vxlanVNI,
		"PLAYPEN_MEMORY=4096M",
		"PLAYPEN_RESOURCE_MEMORY_LIMIT=5Gi",
		"PLAYPEN_MTU=1360",
	}, "make", "playpen-manifests")

	h.kubectl(ctx, "apply", "-f", "deploy/machina/rendered/01-namespace.yaml")
	h.kubectl(ctx, "apply", "-f", "deploy/machina/crd")
	h.kubectl(ctx, "apply", "-f", "deploy/machina/rendered/06-metalman-rbac.yaml")
	h.resetMachineResources(ctx)
	h.kubectl(ctx, "delete", "namespace", playpenNS, "--ignore-not-found", "--wait=true")
	h.kubectl(ctx, "apply", "-f", "deploy/playpen/rendered")
	h.kubectl(ctx, "-n", playpenNS, "rollout", "status", "statefulset/playpen", "--timeout=180s")

	h.podIP = strings.TrimSpace(h.kubectlOutput(ctx, "-n", playpenNS, "get", "pod", "playpen-0", "-o", "jsonpath={.status.podIP}"))
	h.nodeIP = strings.TrimSpace(h.output(ctx, "docker", "inspect", h.node, "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"))
	hostGateway := strings.TrimSpace(h.output(ctx, "docker", "inspect", h.node, "--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}"))
	if h.podIP == "" || h.nodeIP == "" || hostGateway == "" {
		h.t.Fatal("could not determine Playpen and kind network addresses")
	}

	h.run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1")
	h.run(ctx, "sh", "-c", "while iptables -C FORWARD -d 172.30.11.0/30 -j ACCEPT 2>/dev/null; do iptables -D FORWARD -d 172.30.11.0/30 -j ACCEPT; done")
	h.run(ctx, "sh", "-c", "while iptables -C FORWARD -s 172.30.11.0/30 -j ACCEPT 2>/dev/null; do iptables -D FORWARD -s 172.30.11.0/30 -j ACCEPT; done")
	h.run(ctx, "sh", "-c", "while iptables -t nat -C POSTROUTING -s 172.30.11.0/30 ! -d "+h.podIP+"/32 -j MASQUERADE 2>/dev/null; do iptables -t nat -D POSTROUTING -s 172.30.11.0/30 ! -d "+h.podIP+"/32 -j MASQUERADE; done")
	h.run(ctx, "iptables", "-I", "FORWARD", "-d", "172.30.11.0/30", "-j", "ACCEPT")
	h.run(ctx, "iptables", "-I", "FORWARD", "-s", "172.30.11.0/30", "-j", "ACCEPT")
	h.run(ctx, "iptables", "-t", "nat", "-I", "POSTROUTING", "-s", "172.30.11.0/30", "!", "-d", h.podIP+"/32", "-j", "MASQUERADE")
	h.run(ctx, "ip", "route", "replace", h.podIP+"/32", "via", h.nodeIP)
	h.run(ctx, "docker", "exec", h.node, "ip", "route", "replace", "172.30.11.2/32", "via", hostGateway)

	h.writeMetalmanKubeconfig(ctx)
}

func (h *harness) writeMetalmanKubeconfig(ctx context.Context) {
	token := strings.TrimSpace(h.kubectlOutput(ctx, "-n", "unbounded-kube", "create", "token", "metalman-controller", "--duration=2h"))
	admin, err := os.ReadFile(h.kubeconfig)
	if err != nil {
		h.t.Fatal(err)
	}

	var config map[string]any
	if err := json.Unmarshal([]byte(h.outputInput(ctx, admin, "kubectl", "config", "view", "--raw", "-o", "json", "--kubeconfig", "/dev/stdin")), &config); err != nil {
		h.t.Fatal(err)
	}

	clusters := config["clusters"].([]any)
	cluster := clusters[0].(map[string]any)
	clusterConfig := cluster["cluster"].(map[string]any)
	clusterConfig["server"] = "https://" + clusterIP + ":6443"
	config["users"] = []any{map[string]any{"name": "metalman", "user": map[string]any{"token": token}}}
	config["contexts"] = []any{map[string]any{"name": "metalman", "context": map[string]any{"cluster": cluster["name"], "user": "metalman"}}}
	config["current-context"] = "metalman"

	data, err := json.Marshal(config)
	if err != nil {
		h.t.Fatal(err)
	}

	yaml := h.outputInput(ctx, data, "kubectl", "config", "view", "--raw", "--kubeconfig", "/dev/stdin")
	h.writeFile(filepath.Join(h.privateDir, "metalman.kubeconfig"), []byte(yaml), 0o600)
}

func (h *harness) packageAgent() {
	h.t.Helper()

	outPath := filepath.Join(h.privateDir, "unbounded-agent-linux-amd64.tar.gz")
	out, err := os.Create(outPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	data, err := os.ReadFile(filepath.Join(h.root, "bin", "unbounded-agent"))
	if err != nil {
		h.t.Fatal(err)
	}

	header := &tar.Header{Name: "unbounded-agent", Mode: 0o755, Size: int64(len(data))}
	if err := tw.WriteHeader(header); err != nil {
		h.t.Fatal(err)
	}

	if _, err := tw.Write(data); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeSHA256(path string) {
	h.t.Helper()

	file, err := os.Open(path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		h.t.Fatal(err)
	}

	h.writeFile(path+".sha256", []byte(fmt.Sprintf("%x  %s\n", hash.Sum(nil), filepath.Base(path))), 0o644)
}

func (h *harness) startPlaypenClient(ctx context.Context) helperState {
	h.t.Helper()

	testBinary, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}

	logFile, err := os.Create(filepath.Join(h.artifacts, "playpen-client.log"))
	if err != nil {
		h.t.Fatal(err)
	}

	h.client = exec.CommandContext(ctx, filepath.Join(h.root, "bin", "playpen"),
		"client",
		"--namespace", clientNamespace,
		"--endpoint-cidr", clientCIDR,
		"--gateway-ip", clientGateway,
		"--pod-namespace", playpenNS,
		"--kubeconfig", h.kubeconfig,
		"--bridge-cidr", bridgeIP+"/24",
		"--vxlan-vni", vxlanVNI,
		"--", testBinary, "-test.run", "^TestMetalmanSmokeOnPlaypen$", "-test.v",
	)
	h.client.Dir = h.root
	h.client.Env = append(os.Environ(),
		"METALMAN_SMOKE_HELPER=1",
		"METALMAN_SMOKE_ROOT="+h.root,
		"METALMAN_SMOKE_PRIVATE="+h.privateDir,
		"METALMAN_SMOKE_ARTIFACTS="+h.artifacts,
		"METALMAN_SMOKE_KIND_PID="+strings.TrimSpace(h.output(ctx, "docker", "inspect", "-f", "{{.State.Pid}}", h.node)),
	)
	h.client.Stdout = logFile
	h.client.Stderr = logFile
	h.client.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := h.client.Start(); err != nil {
		h.t.Fatal(err)
	}

	statePath := filepath.Join(h.privateDir, "network-ready.json")
	h.waitFile(ctx, "network-ready.json", time.Minute)
	data, err := os.ReadFile(statePath)
	if err != nil {
		h.t.Fatal(err)
	}

	var state helperState
	if err := json.Unmarshal(data, &state); err != nil {
		h.t.Fatal(err)
	}

	if state.IP == "" || state.MAC == "" {
		h.t.Fatalf("invalid helper state: %+v", state)
	}

	return state
}

func (h *harness) configureControlPlane(ctx context.Context) {
	h.t.Helper()

	h.run(ctx, "docker", "exec", h.node, "sh", "-c",
		`flags=$(cat /var/lib/kubelet/kubeadm-flags.env); flags=$(printf '%s' "$flags" | sed -E 's/(^|[[:space:]])--node-ip=[^[:space:]\"]+//g'); printf '%s\n' "${flags%\"} --node-ip=`+clusterIP+`\"" > /var/lib/kubelet/kubeadm-flags.env; systemctl restart kubelet`)
	h.kubectl(ctx, "-n", "kube-system", "set", "env", "daemonset/kindnet", "CONTROL_PLANE_ENDPOINT="+clusterIP+":6443")
	h.waitFor(ctx, "kind control-plane InternalIP", 2*time.Minute, func() bool {
		return strings.TrimSpace(h.kubectlOutputIgnore("get", "node", h.node, "-o", "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}")) == clusterIP
	})
}

func (h *harness) signalControlPlaneReady() {
	h.writeFile(filepath.Join(h.privateDir, "control-plane-ready"), []byte("ready\n"), 0o644)
}

func (h *harness) createMachine(ctx context.Context, state helperState) {
	h.t.Helper()

	secret := h.kubectlOutput(ctx, "-n", "unbounded-kube", "create", "secret", "generic", "smoke-redfish", "--from-literal=password=playpen", "--dry-run=client", "-o", "yaml")
	h.kubectlInput(ctx, []byte(secret), "apply", "-f", "-")

	machine := fmt.Sprintf(`apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: %s
  labels:
    unbounded-cloud.io/site: %s
spec:
  pxe:
    image: %s:5555/unbounded/host-ubuntu2404:smoke
    architecture: amd64
    dhcpLeases:
    - ipv4: %s
      mac: %s
      subnetMask: 255.255.255.0
      gateway: %s
      dns: [%s]
    redfish:
      url: https://%s:8443
      username: admin
      deviceID: "1"
      passwordRef:
        name: smoke-redfish
        namespace: unbounded-kube
        key: password
  kubernetes:
    nodeLabels:
      unbounded-cloud.io/smoke-test: metalman
  agent:
    image: %s:5555/unbounded/agent-ubuntu2404:smoke
    url: http://%s:8881/unbounded-agent-linux-amd64.tar.gz
    containerImageArchives:
    - http://%s:8881/node-images.tar
    downloads:
      kubernetes:
        baseURL: http://%s:8882/kubernetes
      containerd:
        baseURL: http://%s:8882/containerd
      runc:
        baseURL: http://%s:8882/runc
      cni:
        baseURL: http://%s:8882/cni
      crictl:
        baseURL: http://%s:8882/crictl
`, machineName, siteName, bridgeIP, guestIP, state.MAC, bridgeIP, bridgeIP, state.IP, bridgeIP, bridgeIP, bridgeIP, bridgeIP, bridgeIP, bridgeIP, bridgeIP, bridgeIP)
	if strings.ContainsRune(machine, '\t') {
		h.t.Fatal("generated Machine YAML contains a tab")
	}

	h.kubectlInput(ctx, []byte(machine), "apply", "-f", "-")
}

func (h *harness) startDNSRelay(ctx context.Context) {
	h.t.Helper()

	listenAddress := net.JoinHostPort(clientGateway, "53")
	targetAddress := net.JoinHostPort(h.resolver, "53")
	udpListener, err := net.ListenPacket("udp4", listenAddress)
	if err != nil {
		h.t.Fatalf("listen for guest UDP DNS: %v", err)
	}

	tcpListener, err := net.Listen("tcp4", listenAddress)
	if err != nil {
		_ = udpListener.Close()
		h.t.Fatalf("listen for guest TCP DNS: %v", err)
	}

	go relayDNSUDP(ctx, udpListener, targetAddress)
	go func() {
		_ = serveTCPListener(ctx, tcpListener, targetAddress)
	}()
}

func (h *harness) startArtifactProxy(ctx context.Context) {
	h.t.Helper()

	server := &http.Server{
		Addr:              clientGateway + ":8882",
		Handler:           http.HandlerFunc(proxyArtifact),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		_ = server.ListenAndServe()
	}()

	h.waitHTTP(ctx, "http://"+clientGateway+":8882/readyz", time.Minute)
}

func proxyArtifact(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/readyz" {
		writer.WriteHeader(http.StatusOK)

		return
	}

	upstreams := map[string]string{
		"kubernetes": "https://dl.k8s.io",
		"containerd": "https://github.com/containerd/containerd/releases/download",
		"runc":       "https://github.com/opencontainers/runc/releases/download",
		"cni":        "https://github.com/containernetworking/plugins/releases/download",
		"crictl":     "https://github.com/kubernetes-sigs/cri-tools/releases/download",
	}
	parts := strings.SplitN(strings.TrimPrefix(request.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || upstreams[parts[0]] == "" {
		http.NotFound(writer, request)

		return
	}

	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, upstreams[parts[0]]+"/"+parts[1], http.NoBody)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)

		return
	}
	if value := request.Header.Get("Range"); value != "" {
		upstreamRequest.Header.Set("Range", value)
	}

	response, err := http.DefaultClient.Do(upstreamRequest)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)

		return
	}
	defer response.Body.Close()

	for _, name := range []string{"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, response.Body)
	}
}

func relayDNSUDP(ctx context.Context, listener net.PacketConn, targetAddress string) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		request := make([]byte, 64*1024)
		n, client, err := listener.ReadFrom(request)
		if err != nil {
			return
		}

		go func(request []byte, client net.Addr) {
			upstream, dialErr := net.DialTimeout("udp4", targetAddress, 5*time.Second)
			if dialErr != nil {
				return
			}
			defer upstream.Close()

			_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
			if _, writeErr := upstream.Write(request); writeErr != nil {
				return
			}

			response := make([]byte, 64*1024)
			responseSize, readErr := upstream.Read(response)
			if readErr == nil {
				_, _ = listener.WriteTo(response[:responseSize], client)
			}
		}(append([]byte(nil), request[:n]...), client)
	}
}

func hostResolver(t *testing.T) string {
	t.Helper()

	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) != 2 || fields[0] != "nameserver" {
				continue
			}

			ip := net.ParseIP(fields[1])
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				_ = file.Close()

				return fields[1]
			}
		}

		_ = file.Close()
	}

	t.Fatal("no non-loopback IPv4 DNS resolver found")

	return ""
}

func (h *harness) resetMachineResources(ctx context.Context) {
	h.t.Helper()

	for operation := range strings.Lines(h.kubectlOutputIgnore("get", "machineoperations", "-o", "name")) {
		if strings.HasPrefix(operation, "machineoperation.unbounded-cloud.io/"+machineName+"-") {
			h.kubectl(ctx, "delete", operation, "--ignore-not-found", "--wait=true")
		}
	}
	h.kubectl(ctx, "delete", "machine", machineName, "--ignore-not-found", "--wait=true")
	h.kubectl(ctx, "delete", "node", machineName, "--ignore-not-found", "--wait=true")
}

func (h *harness) runReplace(ctx context.Context) {
	h.t.Helper()

	logFile, err := os.Create(filepath.Join(h.artifacts, "host-replace.log"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, filepath.Join(h.root, "bin", "kubectl-unbounded"), "machine", "replace", machineName, "--force", "--ttl=3600")
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}

	h.waitFor(ctx, "CloudInitDone", 6*time.Minute, func() bool {
		value := h.kubectlOutputIgnore("get", "machine", machineName, "-o", "jsonpath={.status.conditions[?(@.type=='CloudInitDone')].status}/{.status.conditions[?(@.type=='CloudInitDone')].reason}")
		if strings.HasPrefix(value, "False/Failed") {
			h.t.Fatalf("CloudInitDone failed: %s", value)
		}

		return value == "True/Succeeded"
	})

	if err := cmd.Wait(); err != nil {
		h.t.Fatalf("kubectl-unbounded machine replace: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(h.artifacts, "host-replace.log"))
	if err != nil {
		h.t.Fatal(err)
	}

	if !strings.Contains(string(data), "Condition CloudInitDone: True/Succeeded") {
		h.t.Fatal("kubectl-unbounded output did not report successful CloudInitDone")
	}
}

func (h *harness) assertNode(ctx context.Context) {
	h.waitFor(ctx, "worker Node Ready", 10*time.Minute, func() bool {
		return h.kubectlOutputIgnore("get", "node", machineName, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}") == "True"
	})

	label := h.kubectlOutput(ctx, "get", "node", machineName, "-o", "jsonpath={.metadata.labels.unbounded-cloud\\.io/smoke-test}")
	if label != "metalman" {
		h.t.Fatalf("unexpected smoke node label %q", label)
	}
}

func (h *harness) runPowerOperations(ctx context.Context) {
	bootID := h.kubectlOutput(ctx, "get", "node", machineName, "-o", "jsonpath={.status.nodeInfo.bootID}")
	h.t.Log("powering the Playpen VM off")
	h.createOperation(ctx, "smoke-power-off", "HostPowerOff", false)
	h.waitOperation(ctx, "smoke-power-off")
	h.waitPower(ctx, "Off")

	h.t.Log("powering the Playpen VM on")
	h.createOperation(ctx, "smoke-power-on", "HostPowerOn", false)
	h.waitOperation(ctx, "smoke-power-on")
	h.waitPower(ctx, "On")
	h.waitBootID(ctx, bootID)
	bootID = h.kubectlOutput(ctx, "get", "node", machineName, "-o", "jsonpath={.status.nodeInfo.bootID}")

	h.t.Log("rebooting the Playpen VM through a site selector")
	h.createOperation(ctx, "smoke-reboot", "HostReboot", true)
	h.waitOperation(ctx, "smoke-reboot")
	h.waitPower(ctx, "On")
	h.waitBootID(ctx, bootID)
}

func (h *harness) createOperation(ctx context.Context, name, kind string, selector bool) {
	target := "  machineRef: " + machineName
	if selector {
		target = "  machineSelector:\n    matchLabels:\n      unbounded-cloud.io/site: " + siteName
	}

	manifest := fmt.Sprintf("apiVersion: unbounded-cloud.io/v1alpha3\nkind: MachineOperation\nmetadata:\n  name: %s\nspec:\n%s\n  operationKind: %s\n", name, target, kind)
	h.kubectlInput(ctx, []byte(manifest), "apply", "-f", "-")
}

func (h *harness) waitOperation(ctx context.Context, name string) {
	h.waitFor(ctx, name+" completion", 5*time.Minute, func() bool {
		phase := h.kubectlOutputIgnore("get", "machineoperation", name, "-o", "jsonpath={.status.phase}")
		if phase == "Failed" {
			h.t.Fatalf("operation %s failed", name)
		}

		return phase == "Complete"
	})
}

func (h *harness) waitPower(ctx context.Context, expected string) {
	h.waitFor(ctx, "Playpen power state "+expected, 2*time.Minute, func() bool {
		cmd := exec.Command("ip", "netns", "exec", clientNamespace, "curl", "-ksS", "-u", "admin:playpen", "https://"+h.podIP+":8443/redfish/v1/Systems/1")
		data, err := cmd.Output()
		if err != nil {
			return false
		}

		var response struct {
			PowerState string `json:"PowerState"`
		}
		if json.Unmarshal(data, &response) != nil {
			return false
		}

		return response.PowerState == expected
	})
}

func (h *harness) waitBootID(ctx context.Context, old string) {
	h.waitFor(ctx, "worker boot ID change", 5*time.Minute, func() bool {
		current := h.kubectlOutputIgnore("get", "node", machineName, "-o", "jsonpath={.status.nodeInfo.bootID}")
		return current != "" && current != old
	})
}

func runHelper() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	privateDir := os.Getenv("METALMAN_SMOKE_PRIVATE")
	root := os.Getenv("METALMAN_SMOKE_ROOT")
	artifacts := os.Getenv("METALMAN_SMOKE_ARTIFACTS")
	kindPID := os.Getenv("METALMAN_SMOKE_KIND_PID")
	state := helperState{
		Pod: os.Getenv("PLAYPEN_CLAIMED_POD"),
		IP:  os.Getenv("PLAYPEN_REMOTE_IP"),
		MAC: os.Getenv("PLAYPEN_VM_MAC"),
	}
	if state.IP == "" || state.MAC == "" || kindPID == "" {
		return fmt.Errorf("missing Playpen claim or kind PID: %+v", state)
	}
	deleteKindLink := exec.CommandContext(ctx, "nsenter", "-t", kindPID, "-n", "ip", "link", "delete", "eth-smoke")
	_ = deleteKindLink.Run()

	commands := [][]string{
		{"ip", "link", "add", "kind-smoke", "type", "veth", "peer", "name", "eth-smoke"},
		{"ip", "link", "set", "kind-smoke", "mtu", "1360"},
		{"ip", "link", "set", "kind-smoke", "master", "br-playpen"},
		{"ip", "link", "set", "kind-smoke", "up"},
		{"ip", "link", "set", "eth-smoke", "netns", kindPID},
		{"nsenter", "-t", kindPID, "-n", "ip", "addr", "replace", clusterIP + "/24", "dev", "eth-smoke"},
		{"nsenter", "-t", kindPID, "-n", "ip", "link", "set", "eth-smoke", "mtu", "1360"},
		{"nsenter", "-t", kindPID, "-n", "ip", "link", "set", "eth-smoke", "up"},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "192.168.200.0/24", "-o", "underlay0", "-j", "MASQUERADE"},
		{"iptables", "-t", "mangle", "-A", "FORWARD", "-i", "br-playpen", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--set-mss", "1320"},
		{"iptables", "-A", "FORWARD", "-i", "br-playpen", "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-o", "br-playpen", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if data, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, data)
		}
	}

	if err := writeJSONAtomic(filepath.Join(privateDir, "network-ready.json"), state); err != nil {
		return err
	}

	if err := waitPath(ctx, filepath.Join(privateDir, "control-plane-ready"), 3*time.Minute); err != nil {
		return err
	}

	dnsListener, err := net.ListenPacket("udp4", net.JoinHostPort(bridgeIP, "53"))
	if err != nil {
		return fmt.Errorf("listen for guest DNS: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		_ = serveTCP(ctx, bridgeIP+":5555", clientGateway+":5555")
	}()
	go func() {
		defer wg.Done()
		_ = serveTCP(ctx, bridgeIP+":8882", clientGateway+":8882")
	}()
	go func() {
		defer wg.Done()
		relayDNSUDP(ctx, dnsListener, net.JoinHostPort(clientGateway, "53"))
	}()
	go func() {
		defer wg.Done()
		_ = serveTCP(ctx, net.JoinHostPort(bridgeIP, "53"), net.JoinHostPort(clientGateway, "53"))
	}()
	go func() {
		defer wg.Done()
		server := &http.Server{Addr: bridgeIP + ":8881", Handler: http.FileServer(http.Dir(privateDir)), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			_ = server.Close()
		}()
		_ = server.ListenAndServe()
	}()

	metalmanLog, err := os.Create(filepath.Join(artifacts, "metalman.log"))
	if err != nil {
		return err
	}
	defer metalmanLog.Close()

	metalman := exec.CommandContext(ctx, filepath.Join(root, "bin", "metalman"),
		"serve-pxe",
		"--site="+siteName,
		"--bind-address="+bridgeIP,
		"--cache-dir="+filepath.Join(privateDir, "cache"),
		"--serve-url=http://"+bridgeIP+":8880",
		"--dhcp-interface=br-playpen",
		"--health-port=8085",
		"--default-netboot-image="+bridgeIP+":5555/unbounded/netboot:smoke",
		"--leader-elect-lease-duration=60s",
		"--leader-elect-renew-deadline=40s",
		"--leader-elect-retry-period=5s",
		"--operation-poll-interval=2s",
	)
	metalman.Env = append(os.Environ(),
		"KUBECONFIG="+filepath.Join(privateDir, "metalman.kubeconfig"),
		"METALMAN_APISERVER_URL=https://"+clusterIP+":6443",
	)
	metalman.Stdout = metalmanLog
	metalman.Stderr = metalmanLog
	if err := metalman.Start(); err != nil {
		return err
	}

	if err := waitURL(ctx, "http://127.0.0.1:8085/readyz", 2*time.Minute); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(privateDir, "metalman-ready"), []byte("ready\n"), 0o644); err != nil {
		return err
	}

	err = metalman.Wait()
	cancel()
	wg.Wait()

	return err
}

func serveTCP(ctx context.Context, listenAddress, targetAddress string) error {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}

	return serveTCPListener(ctx, listener, targetAddress)
}

func serveTCPListener(ctx context.Context, listener net.Listener, targetAddress string) error {
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		client, err := listener.Accept()
		if err != nil {
			return err
		}

		go func(client net.Conn) {
			upstream, dialErr := net.Dial("tcp", targetAddress)
			if dialErr != nil {
				_ = client.Close()

				return
			}

			go func() {
				_, _ = io.Copy(upstream, client)
				_ = upstream.Close()
			}()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
		}(client)
	}
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}

	return os.Rename(temporary, path)
}

func waitPath(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", path)
		case <-time.After(time.Second):
		}
	}
}

func waitURL(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode < http.StatusInternalServerError {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", url)
		case <-time.After(time.Second):
		}
	}
}

func urlReady(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return false
	}
	defer response.Body.Close()

	return response.StatusCode < http.StatusInternalServerError
}

func (h *harness) cleanup() {
	if h.client != nil && h.client.Process != nil {
		_ = syscall.Kill(-h.client.Process.Pid, syscall.SIGTERM)
		_, _ = h.client.Process.Wait()
	}

	h.runIgnore("ip", "netns", "delete", clientNamespace)
	if h.node != "" {
		h.runIgnore("docker", "exec", h.node, "ip", "link", "delete", "eth-smoke")
	}
	h.runIgnore("iptables", "-D", "FORWARD", "-d", "172.30.11.0/30", "-j", "ACCEPT")
	h.runIgnore("iptables", "-D", "FORWARD", "-s", "172.30.11.0/30", "-j", "ACCEPT")
	h.runIgnore("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", "172.30.11.0/30", "!", "-d", h.podIP+"/32", "-j", "MASQUERADE")
	if h.podIP != "" {
		h.runIgnore("ip", "route", "delete", h.podIP+"/32", "via", h.nodeIP)
	}

	if h.registry {
		h.runIgnore("docker", "rm", "-f", "unbounded-metalman-smoke-registry")
	}
}

func (h *harness) collectDiagnostics() {
	commands := map[string][]string{
		"all.txt":           {"get", "pods,nodes,machines,machineoperations", "-A", "-o", "wide"},
		"events.txt":        {"get", "events", "-A", "--sort-by=.lastTimestamp"},
		"playpen-pod.txt":   {"-n", playpenNS, "describe", "pod", "playpen-0"},
		"playpen.log":       {"-n", playpenNS, "logs", "playpen-0"},
		"smoke-node.txt":    {"describe", "node", machineName},
		"smoke-machine.txt": {"get", "machine", machineName, "-o", "yaml"},
	}
	for name, args := range commands {
		data := h.kubectlOutputIgnore(args...)
		_ = os.WriteFile(filepath.Join(h.artifacts, name), []byte(data), 0o644)
	}
}

func (h *harness) waitFile(ctx context.Context, name string, timeout time.Duration) {
	h.t.Helper()

	if err := waitPath(ctx, filepath.Join(h.privateDir, name), timeout); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) waitHTTP(ctx context.Context, url string, timeout time.Duration) {
	h.t.Helper()

	if err := waitURL(ctx, url, timeout); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) waitFor(ctx context.Context, description string, timeout time.Duration, condition func() bool) {
	h.t.Helper()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		if condition() {
			return
		}

		select {
		case <-ctx.Done():
			h.t.Fatal(ctx.Err())
		case <-deadline.C:
			h.t.Fatalf("timed out waiting for %s", description)
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *harness) run(ctx context.Context, name string, args ...string) {
	h.t.Helper()

	if _, err := h.command(ctx, nil, nil, name, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) runEnv(ctx context.Context, env []string, name string, args ...string) {
	h.t.Helper()

	if _, err := h.command(ctx, env, nil, name, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) output(ctx context.Context, name string, args ...string) string {
	h.t.Helper()

	data, err := h.command(ctx, nil, nil, name, args...)
	if err != nil {
		h.t.Fatal(err)
	}

	return string(data)
}

func (h *harness) outputInput(ctx context.Context, input []byte, name string, args ...string) string {
	h.t.Helper()

	data, err := h.command(ctx, nil, input, name, args...)
	if err != nil {
		h.t.Fatal(err)
	}

	return string(data)
}

func (h *harness) command(ctx context.Context, env []string, input []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), env...)
	if input != nil {
		cmd.Stdin = strings.NewReader(string(input))
	}

	data, err := cmd.CombinedOutput()
	if err != nil {
		return data, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, data)
	}

	return data, nil
}

func (h *harness) runIgnore(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = h.root
	_ = cmd.Run()
}

func (h *harness) kubectl(ctx context.Context, args ...string) {
	h.t.Helper()

	allArgs := append([]string{"--kubeconfig", h.kubeconfig}, args...)
	h.run(ctx, "kubectl", allArgs...)
}

func (h *harness) kubectlOutput(ctx context.Context, args ...string) string {
	h.t.Helper()

	allArgs := append([]string{"--kubeconfig", h.kubeconfig}, args...)
	return strings.TrimSpace(h.output(ctx, "kubectl", allArgs...))
}

func (h *harness) kubectlOutputIgnore(args ...string) string {
	allArgs := append([]string{"--kubeconfig", h.kubeconfig}, args...)
	cmd := exec.Command("kubectl", allArgs...)
	cmd.Dir = h.root
	data, _ := cmd.CombinedOutput()

	return strings.TrimSpace(string(data))
}

func (h *harness) kubectlInput(ctx context.Context, input []byte, args ...string) {
	h.t.Helper()

	allArgs := append([]string{"--kubeconfig", h.kubeconfig}, args...)
	if _, err := h.command(ctx, nil, input, "kubectl", allArgs...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeFile(path string, data []byte, mode os.FileMode) {
	h.t.Helper()

	if err := os.WriteFile(path, data, mode); err != nil {
		h.t.Fatal(err)
	}
}
