// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalredfish "github.com/Azure/unbounded/internal/metalman/redfish"
	"github.com/Azure/unbounded/internal/playpen/meta"
)

type recordedCommand struct {
	Name string
	Args []string
}

type fakeCommander struct {
	mu        sync.Mutex
	runs      []recordedCommand
	starts    []recordedCommand
	startErr  error
	startHook func(recordedCommand) error
}

func (f *fakeCommander) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs = append(f.runs, recordedCommand{Name: name, Args: append([]string(nil), args...)})

	return nil
}

func (f *fakeCommander) Start(_ context.Context, name string, args []string, _, _ string) (Process, error) {
	cmd := recordedCommand{Name: name, Args: append([]string(nil), args...)}

	f.mu.Lock()
	f.starts = append(f.starts, cmd)
	startErr := f.startErr
	startHook := f.startHook
	f.mu.Unlock()

	if startErr != nil {
		return nil, startErr
	}

	if startHook != nil {
		if err := startHook(cmd); err != nil {
			return nil, err
		}
	}

	return &fakeProcess{pid: len(f.starts)}, nil
}

func (f *fakeCommander) runStrings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	commands := make([]string, 0, len(f.runs))
	for _, cmd := range f.runs {
		commands = append(commands, cmd.Name+" "+strings.Join(cmd.Args, " "))
	}

	return commands
}

func (f *fakeCommander) startStrings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	commands := make([]string, 0, len(f.starts))
	for _, cmd := range f.starts {
		commands = append(commands, cmd.Name+" "+strings.Join(cmd.Args, " "))
	}

	return commands
}

type fakeProcess struct {
	pid    int
	exited bool
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Exited() bool { return p.exited }

func (p *fakeProcess) Signal(os.Signal) error {
	p.exited = true

	return nil
}

func (p *fakeProcess) Kill() error {
	p.exited = true

	return nil
}

func (p *fakeProcess) Wait() error { return nil }

func TestNetworkSetupCommands(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WireGuard.ClientPublicKey = "client-public-key"
	fake := &fakeCommander{}

	manager := NewNetworkManager(fake, cfg)
	if err := manager.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	commands := strings.Join(fake.runStrings(), "\n")
	for _, want := range []string{
		"ip link add wg0 type wireguard",
		"wg set wg0 private-key /etc/playpen/wireguard/privatekey listen-port 51820",
		"wg set wg0 peer client-public-key allowed-ips 10.88.0.2/32",
		"ip link add br0 type bridge",
		"ip link add vxlan0 type vxlan id 12001 dev wg0 local 10.88.0.1 remote 10.88.0.2 dstport 4789 nolearning",
		"bridge fdb append 00:00:00:00:00:00 dev vxlan0 dst 10.88.0.2",
		"ip tuntap add dev tap0 mode tap",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}

	for _, forbidden := range []string{
		"192.168.200.1",
		"MASQUERADE",
		"net.ipv4.ip_forward",
		"iptables",
	} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("runner network setup contains %q, want VM egress isolated to VXLAN:\n%s", forbidden, commands)
		}
	}
}

func TestNetworkSetupWithoutInitialPeer(t *testing.T) {
	cfg := DefaultConfig()
	fake := &fakeCommander{}

	manager := NewNetworkManager(fake, cfg)
	if err := manager.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	commands := strings.Join(fake.runStrings(), "\n")
	if strings.Contains(commands, " peer ") {
		t.Fatalf("setup configured peer before claim:\n%s", commands)
	}

	if err := manager.ConfigurePeer(t.Context(), "client-public-key"); err != nil {
		t.Fatalf("configure peer: %v", err)
	}

	commands = strings.Join(fake.runStrings(), "\n")
	if !strings.Contains(commands, "wg set wg0 peer client-public-key allowed-ips 10.88.0.2/32") {
		t.Fatalf("commands missing delayed peer:\n%s", commands)
	}
}

func TestWaitForClientPublicKeyAnnotationConfiguresPeer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfigureNetwork = true
	cfg.PodName = "runner-1"
	cfg.PodNamespace = "playpen"
	cfg.KubernetesClient = testKubernetesClient(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.PodName, Namespace: cfg.PodNamespace},
	})
	fake := &fakeCommander{}
	manager := NewNetworkManager(fake, cfg)
	state := NewRuntimeState("server-public-key", false)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go waitForClientPublicKeyAnnotation(ctx, cfg, manager, state)

	if state.Ready() {
		t.Fatal("state is ready before claim")
	}

	pod := &corev1.Pod{}
	if err := cfg.KubernetesClient.Get(t.Context(), client.ObjectKey{Namespace: cfg.PodNamespace, Name: cfg.PodName}, pod); err != nil {
		t.Fatal(err)
	}

	base := pod.DeepCopy()

	pod.Annotations = map[string]string{meta.AnnotationClientWireGuardPublicKey: "client-public-key"}
	if err := cfg.KubernetesClient.Patch(t.Context(), pod, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !state.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if !state.Ready() {
		t.Fatal("state did not become ready")
	}

	commands := strings.Join(fake.runStrings(), "\n")
	if !strings.Contains(commands, "wg set wg0 peer client-public-key allowed-ips 10.88.0.2/32") {
		t.Fatalf("commands missing delayed peer:\n%s", commands)
	}
}

func TestInfoResponseUsesWireGuardAddressForDefaultRedfishURL(t *testing.T) {
	cfg := DefaultConfig()
	info := infoResponse(cfg, "server-public-key")
	redfish := info["redfish"].(map[string]string)

	if got, want := redfish["url"], "https://10.88.0.1:8443"; got != want {
		t.Fatalf("redfish url = %q, want %q", got, want)
	}
}

func TestVMManagerQEMUCommands(t *testing.T) {
	cfg := testConfig(t)
	cfg.QEMU.EnableTPM = true

	var swtpmSocket net.Listener

	fake := &fakeCommander{
		startHook: func(cmd recordedCommand) error {
			if cmd.Name != cfg.QEMU.SWTPMBinary {
				return nil
			}

			listener, err := net.Listen("unix", filepath.Join(os.TempDir(), "playpen-runner-swtpm.sock"))
			if err != nil {
				return err
			}

			swtpmSocket = listener

			return nil
		},
	}

	t.Cleanup(func() {
		if swtpmSocket != nil {
			_ = swtpmSocket.Close()
		}

		_ = os.Remove(filepath.Join(os.TempDir(), "playpen-runner-swtpm.sock"))
	})

	vm := NewVMManager(fake, cfg)

	if err := vm.Reset(t.Context(), ResetOn); err != nil {
		t.Fatalf("reset on: %v", err)
	}

	if state := vm.PowerState(); state != PowerOn {
		t.Fatalf("power state = %s, want %s", state, PowerOn)
	}

	starts := strings.Join(fake.startStrings(), "\n")
	for _, want := range []string{
		"swtpm socket --tpm2 --runas 0",
		"qemu-system-x86_64 -enable-kvm",
		"-machine q35,accel=kvm",
		"-netdev tap,id=net0,ifname=tap0,script=no,downscript=no",
		"-device virtio-net-pci,netdev=net0,mac=52:54:00:aa:bb:01",
		"-boot order=n",
		"-device tpm-tis,tpmdev=tpm0",
	} {
		if !strings.Contains(starts, want) {
			t.Fatalf("start commands missing %q:\n%s", want, starts)
		}
	}

	if strings.Contains(starts, "-no-reboot") {
		t.Fatalf("qemu command disables guest reboot:\n%s", starts)
	}

	if err := vm.Reset(t.Context(), ResetForceOff); err != nil {
		t.Fatalf("reset force off: %v", err)
	}

	if state := vm.PowerState(); state != PowerOff {
		t.Fatalf("power state = %s, want %s", state, PowerOff)
	}
}

func TestVMManagerQEMUCommandsForARM64(t *testing.T) {
	cfg := testConfig(t)
	cfg.Architecture = ArchitectureARM64

	cfg.QEMU.EnableTPM = true
	if err := cfg.ApplyArchitectureDefaults(); err != nil {
		t.Fatalf("apply architecture defaults: %v", err)
	}

	if err := os.WriteFile(cfg.QEMU.OVMFVarsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}

	var swtpmSocket net.Listener

	fake := &fakeCommander{
		startHook: func(cmd recordedCommand) error {
			if cmd.Name != cfg.QEMU.SWTPMBinary {
				return nil
			}

			listener, err := net.Listen("unix", filepath.Join(os.TempDir(), "playpen-runner-swtpm.sock"))
			if err != nil {
				return err
			}

			swtpmSocket = listener

			return nil
		},
	}

	t.Cleanup(func() {
		if swtpmSocket != nil {
			_ = swtpmSocket.Close()
		}

		_ = os.Remove(filepath.Join(os.TempDir(), "playpen-runner-swtpm.sock"))
	})

	vm := NewVMManager(fake, cfg)

	if err := vm.Reset(t.Context(), ResetOn); err != nil {
		t.Fatalf("reset on: %v", err)
	}

	starts := strings.Join(fake.startStrings(), "\n")
	for _, want := range []string{
		"qemu-system-aarch64 -enable-kvm",
		"-machine virt,accel=kvm",
		"-cpu host",
		"-drive if=pflash,format=raw,readonly=on,file=/usr/share/AAVMF/AAVMF_CODE.fd",
		"-device virtio-net-pci,netdev=net0,mac=52:54:00:aa:bb:01",
		"-device tpm-tis-device,tpmdev=tpm0",
	} {
		if !strings.Contains(starts, want) {
			t.Fatalf("start commands missing %q:\n%s", want, starts)
		}
	}
}

func TestRedfishWithMetalmanClient(t *testing.T) {
	cfg := testConfig(t)
	cfg.QEMU.EnableTPM = false
	fake := &fakeCommander{}
	vm := NewVMManager(fake, cfg)
	handler := NewRedfishHandler(vm, cfg.Redfish, cfg.Guest.MAC, cfg.DataDir)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	client, err := metalredfish.Dial(t.Context(), server.URL, tlsServerFingerprint(server), cfg.Redfish.Username, cfg.Redfish.Password, cfg.Redfish.DeviceID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(client.Close)

	state, err := client.PowerState(t.Context())
	if err != nil {
		t.Fatalf("power state: %v", err)
	}

	if state != metalredfish.PowerOff {
		t.Fatalf("power state = %s, want %s", state, metalredfish.PowerOff)
	}

	if err := client.SetBootOverride(t.Context(), metalredfish.BootTargetPxe, metalredfish.BootContinuous); err != nil {
		t.Fatalf("set boot override: %v", err)
	}

	if err := client.Reset(t.Context(), metalredfish.ResetOn); err != nil {
		t.Fatalf("reset on: %v", err)
	}

	state, err = client.PowerState(t.Context())
	if err != nil {
		t.Fatalf("power state after on: %v", err)
	}

	if state != metalredfish.PowerOn {
		t.Fatalf("power state = %s, want %s", state, metalredfish.PowerOn)
	}

	if err := client.DisableBootOverride(t.Context()); err != nil {
		t.Fatalf("disable boot override: %v", err)
	}

	boot, err := client.GetBootConfig(t.Context())
	if err != nil {
		t.Fatalf("get boot config: %v", err)
	}

	if boot.Enabled != metalredfish.BootDisabled {
		t.Fatalf("boot enabled = %s, want %s", boot.Enabled, metalredfish.BootDisabled)
	}

	if err := client.Reset(t.Context(), metalredfish.ResetForceOff); err != nil {
		t.Fatalf("reset force off: %v", err)
	}
}

func TestRedfishHTTPBootWithMetalmanClient(t *testing.T) {
	cfg := testConfig(t)
	cfg.QEMU.EnableTPM = false
	vm := NewVMManager(&fakeCommander{}, cfg)
	handler := NewRedfishHandler(vm, cfg.Redfish, cfg.Guest.MAC, cfg.DataDir)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	client, err := metalredfish.Dial(t.Context(), server.URL, tlsServerFingerprint(server), cfg.Redfish.Username, cfg.Redfish.Password, cfg.Redfish.DeviceID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(client.Close)

	// The emulator always advertises HttpBootUri, so the client detects standard
	// Redfish HTTP boot support.
	boot, err := client.GetBootConfig(t.Context())
	if err != nil {
		t.Fatalf("get boot config: %v", err)
	}

	if !boot.HasHTTPBootURI {
		t.Fatal("expected HasHTTPBootURI to be true")
	}

	staticConfig := metalredfish.StaticIPv4Config{
		MAC:        cfg.Guest.MAC,
		Address:    "192.168.200.10",
		SubnetMask: "255.255.255.0",
		Gateway:    "192.168.200.1",
		DNS:        []string{"8.8.8.8"},
	}
	if err := client.SetStaticIPv4(t.Context(), staticConfig); err != nil {
		t.Fatalf("set static IPv4: %v", err)
	}

	if nic := vm.NICStatic(); !nic.Applied || nic.Address != staticConfig.Address || nic.DHCPEnabled {
		t.Fatalf("NIC static config not applied as expected: %+v", nic)
	}

	const bootURL = "http://192.168.200.1/boot/machine-1.efi"
	if err := client.SetHTTPBootOverride(t.Context(), bootURL); err != nil {
		t.Fatalf("set HTTP boot override: %v", err)
	}

	boot, err = client.GetBootConfig(t.Context())
	if err != nil {
		t.Fatalf("get boot config after HTTP override: %v", err)
	}

	if boot.Target != metalredfish.BootTargetUefiHTTP {
		t.Fatalf("boot target = %s, want %s", boot.Target, metalredfish.BootTargetUefiHTTP)
	}

	if boot.Enabled != metalredfish.BootContinuous {
		t.Fatalf("boot enabled = %s, want %s", boot.Enabled, metalredfish.BootContinuous)
	}

	if boot.Mode != metalredfish.BootModeUEFI {
		t.Fatalf("boot mode = %s, want %s", boot.Mode, metalredfish.BootModeUEFI)
	}

	if boot.UefiHTTPSource != bootURL {
		t.Fatalf("UEFI HTTP source = %q, want %q", boot.UefiHTTPSource, bootURL)
	}

	// The vendor BIOS settings path must also be accepted.
	if err := client.SetBIOSStaticIPv4(t.Context(), staticConfig); err != nil {
		t.Fatalf("set BIOS static IPv4: %v", err)
	}

	if err := client.SetBIOSHTTPBootURI(t.Context(), bootURL); err != nil {
		t.Fatalf("set BIOS HTTP boot URI: %v", err)
	}

	uri, err := client.GetBIOSHTTPBootURI(t.Context())
	if err != nil {
		t.Fatalf("get BIOS HTTP boot URI: %v", err)
	}

	if uri != bootURL {
		t.Fatalf("BIOS HTTP boot URI = %q, want %q", uri, bootURL)
	}

	// The BIOS fallback branch issues a one-shot UEFI HTTP boot override.
	if err := client.SetBootOverride(t.Context(), metalredfish.BootTargetUefiHTTP, metalredfish.BootOnce); err != nil {
		t.Fatalf("set one-shot UEFI HTTP boot override: %v", err)
	}

	if boot, err := client.GetBootConfig(t.Context()); err != nil {
		t.Fatalf("get boot config after one-shot override: %v", err)
	} else if boot.Enabled != metalredfish.BootOnce {
		t.Fatalf("boot enabled = %s, want %s", boot.Enabled, metalredfish.BootOnce)
	}
}

func TestRedfishSystemAdvertisesSerialConsole(t *testing.T) {
	cfg := testConfig(t)
	vm := NewVMManager(&fakeCommander{}, cfg)
	server := httptest.NewServer(NewRedfishHandler(vm, cfg.Redfish, cfg.Guest.MAC, cfg.DataDir))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/redfish/v1/Systems/"+cfg.Redfish.DeviceID, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(cfg.Redfish.Username, cfg.Redfish.Password)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body struct {
		ODataType     string `json:"@odata.type"`
		ID            string `json:"Id"`
		Name          string `json:"Name"`
		SerialConsole struct {
			ServiceEnabled        bool     `json:"ServiceEnabled"`
			MaxConcurrentSessions int      `json:"MaxConcurrentSessions"`
			ConnectTypesSupported []string `json:"ConnectTypesSupported"`
			Oem                   struct {
				Unbounded struct {
					ReadOnly  bool   `json:"ReadOnly"`
					Protocol  string `json:"Protocol"`
					StreamURI string `json:"StreamURI"`
				} `json:"Unbounded"`
			} `json:"Oem"`
		} `json:"SerialConsole"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.ODataType != "#ComputerSystem.v1_20_0.ComputerSystem" {
		t.Fatalf("@odata.type = %q", body.ODataType)
	}

	if body.ID != cfg.Redfish.DeviceID || body.Name == "" {
		t.Fatalf("unexpected system identity: id=%q name=%q", body.ID, body.Name)
	}

	if !body.SerialConsole.ServiceEnabled {
		t.Fatal("serial console service is not enabled")
	}

	if body.SerialConsole.MaxConcurrentSessions != 1 {
		t.Fatalf("max sessions = %d, want 1", body.SerialConsole.MaxConcurrentSessions)
	}

	if len(body.SerialConsole.ConnectTypesSupported) != 1 || body.SerialConsole.ConnectTypesSupported[0] != "OEM" {
		t.Fatalf("connect types = %#v, want OEM", body.SerialConsole.ConnectTypesSupported)
	}

	if !body.SerialConsole.Oem.Unbounded.ReadOnly {
		t.Fatal("serial console stream is not marked read-only")
	}

	if body.SerialConsole.Oem.Unbounded.Protocol != "WebSocket" {
		t.Fatalf("protocol = %q, want WebSocket", body.SerialConsole.Oem.Unbounded.Protocol)
	}

	wantURI := "/redfish/v1/Systems/" + cfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream"
	if body.SerialConsole.Oem.Unbounded.StreamURI != wantURI {
		t.Fatalf("stream URI = %q, want %q", body.SerialConsole.Oem.Unbounded.StreamURI, wantURI)
	}
}

func TestRedfishSerialConsoleStreamRequiresAuth(t *testing.T) {
	cfg := testConfig(t)
	vm := NewVMManager(&fakeCommander{}, cfg)
	server := httptest.NewServer(NewRedfishHandler(vm, cfg.Redfish, cfg.Guest.MAC, cfg.DataDir))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/redfish/v1/Systems/" + cfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestRedfishSerialConsoleStreamFollowsSerialLog(t *testing.T) {
	cfg := testConfig(t)
	vm := NewVMManager(&fakeCommander{}, cfg)
	server := httptest.NewServer(NewRedfishHandler(vm, cfg.Redfish, cfg.Guest.MAC, cfg.DataDir))
	t.Cleanup(server.Close)

	if err := os.WriteFile(filepath.Join(cfg.DataDir, "serial.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/redfish/v1/Systems/" + cfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream"

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: redfishBasicAuthHeader(cfg)})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // Test cleanup.

	file, err := os.OpenFile(filepath.Join(cfg.DataDir, "serial.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := file.WriteString("booting\n"); err != nil {
		_ = file.Close()

		t.Fatal(err)
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	messageType, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if messageType != websocket.MessageBinary {
		t.Fatalf("message type = %v, want binary", messageType)
	}

	if string(data) != "booting\n" {
		t.Fatalf("stream data = %q, want serial log bytes", string(data))
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()

	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.ConfigureNetwork = false
	cfg.Redfish.Username = "admin"
	cfg.Redfish.Password = "secret"

	cfg.QEMU.OVMFVarsTemplate = filepath.Join(cfg.DataDir, "OVMF_VARS_TEMPLATE.fd")
	if err := os.WriteFile(cfg.QEMU.OVMFVarsTemplate, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}

	return cfg
}

func tlsServerFingerprint(server *httptest.Server) string {
	cert := server.Certificate()
	sum := sha256.Sum256(cert.Raw)

	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}

	return strings.Join(parts, ":")
}

func redfishBasicAuthHeader(cfg Config) http.Header {
	header := http.Header{}
	token := base64.StdEncoding.EncodeToString([]byte(cfg.Redfish.Username + ":" + cfg.Redfish.Password))
	header.Set("Authorization", "Basic "+token)

	return header
}

func testKubernetesClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
