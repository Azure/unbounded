// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	playpenapi "github.com/Azure/unbounded/internal/playpen/server"
)

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

type (
	AllocateOptions = playpenapi.AllocateRequest
	Allocation      playpenapi.AllocateResponse
)

type TunnelOptions struct {
	Namespace        string
	Setup            bool
	IPCommand        string
	WGCommand        string
	UnderlayCIDR     string
	WireGuardPort    int
	VXLANInterface   string
	WireGuardIface   string
	AllowEndpoint    bool
	Masquerade       bool
	IPTablesCommand  string
	SysctlCommand    string
	VMPodCIDR        string
	AdditionalRoutes []string
}

type Tunnel struct {
	Allocation *Allocation
	Namespace  string
	ipCommand  string
	wgCommand  string
	iptables   string
	sysctl     string
	underlay   string
	listenPort int
	wgIface    string
	wgHostName string
	vxlanIface string
	hostVeth   string
	nsVeth     string
	hostIP     string
	nsIP       string
	vmPodCIDR  string
	routes     []string
	endpoint   bool
	masquerade bool
	fwmark     int
	table      int
	rulePrio   int
	created    bool
}

func New(config *rest.Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("rest config is required")
	}

	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes http client: %w", err)
	}
	baseURL, err := url.Parse(config.Host)
	if err != nil {
		return nil, fmt.Errorf("parse kubernetes host: %w", err)
	}

	return &Client{baseURL: baseURL, httpClient: httpClient}, nil
}

func (c *Client) Allocate(ctx context.Context, opts AllocateOptions) (*Allocation, error) {
	if opts.ClientWireGuardPublicKey == "" {
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return nil, fmt.Errorf("generate wireguard key: %w", err)
		}

		opts.ClientWireGuardPublicKey = key.PublicKey().String()
		if opts.ClientInternalIP == "" {
			opts.ClientInternalIP = clientUnderlayIP(opts.ClientWireGuardPublicKey)
		}

		var allocation playpenapi.AllocateResponse
		if err := c.post(ctx, playpenapi.AllocatePath, opts, &allocation); err != nil {
			return nil, err
		}
		allocation.Tunnel.WireGuardPrivateKey = key.String()
		allocation.Tunnel.WireGuardPublicKey = opts.ClientWireGuardPublicKey
		converted := Allocation(allocation)

		return &converted, nil
	}
	if opts.ClientInternalIP == "" {
		opts.ClientInternalIP = clientUnderlayIP(opts.ClientWireGuardPublicKey)
	}

	var allocation playpenapi.AllocateResponse
	if err := c.post(ctx, playpenapi.AllocatePath, opts, &allocation); err != nil {
		return nil, err
	}

	converted := Allocation(allocation)

	return &converted, nil
}

func (c *Client) Deallocate(ctx context.Context, allocationID string) error {
	var response playpenapi.DeallocateResponse
	return c.post(ctx, playpenapi.DeallocatePath, playpenapi.DeallocateRequest{AllocationID: allocationID}, &response)
}

func (a *Allocation) Machine(name, namespace, site, image, passwordSecretName, passwordSecretKey string) *machinav1alpha3.Machine {
	if name == "" {
		name = a.AllocationID
	}
	if namespace == "" {
		namespace = a.Namespace
	}
	if passwordSecretName == "" {
		passwordSecretName = a.AllocationID + "-redfish"
	}
	if site == "" {
		site = a.Site
	}
	if passwordSecretKey == "" {
		passwordSecretKey = "password"
	}

	labels := map[string]string{}
	if site != "" {
		labels[machinav1alpha3.MachineSiteLabelKey] = site
	}

	return &machinav1alpha3.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: machinav1alpha3.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: machinav1alpha3.MachineSpec{
			PXE: &machinav1alpha3.PXESpec{
				Image:        image,
				Architecture: machinav1alpha3.PXEArchitectureAMD64,
				DHCPLeases: []machinav1alpha3.DHCPLease{{
					MAC:        a.MACAddress,
					IPv4:       a.Lease.IP,
					SubnetMask: subnetMask(a.Lease.Subnet),
					Gateway:    a.Lease.Router,
					DNS:        []string{a.Lease.DNS},
				}},
				Redfish: &machinav1alpha3.RedfishSpec{
					URL:         a.Redfish.URL,
					Username:    a.Redfish.Username,
					DeviceID:    a.Redfish.DeviceID,
					PasswordRef: machinav1alpha3.SecretKeySelector{Name: passwordSecretName, Namespace: namespace, Key: passwordSecretKey},
				},
			},
		},
	}
}

func (a *Allocation) RedfishSecret(name, namespace, key string) *corev1.Secret {
	if name == "" {
		name = a.AllocationID + "-redfish"
	}
	if namespace == "" {
		namespace = a.Namespace
	}
	if key == "" {
		key = "password"
	}

	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		StringData: map[string]string{key: a.Redfish.Password},
	}
}

func (c *Client) post(ctx context.Context, path string, request any, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	u := *c.baseURL
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr playpenapi.ErrorResponse
		if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("playpen API returned %s: %s", resp.Status, apiErr.Error)
		}

		return fmt.Errorf("playpen API returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	if response == nil {
		return nil
	}
	if err := json.Unmarshal(data, response); err != nil {
		return fmt.Errorf("decode playpen response: %w", err)
	}

	return nil
}

func (a *Allocation) EstablishTunnel(ctx context.Context, opts TunnelOptions) (*Tunnel, error) {
	ns := opts.Namespace
	if ns == "" {
		ns = "playpen-" + a.AllocationID
	}
	ipCommand := opts.IPCommand
	if ipCommand == "" {
		ipCommand = "ip"
	}
	wgCommand := opts.WGCommand
	if wgCommand == "" {
		wgCommand = "wg"
	}
	iptablesCommand := opts.IPTablesCommand
	if iptablesCommand == "" {
		iptablesCommand = "iptables"
	}
	sysctlCommand := opts.SysctlCommand
	if sysctlCommand == "" {
		sysctlCommand = "sysctl"
	}
	underlay := opts.UnderlayCIDR
	if underlay == "" {
		underlay = firstNonEmpty(a.Tunnel.WireGuardAddress, clientUnderlayIP(a.AllocationID)) + "/32"
	}
	wgIface := opts.WireGuardIface
	if wgIface == "" {
		wgIface = "wg0"
	}
	vxlanIface := opts.VXLANInterface
	if vxlanIface == "" {
		vxlanIface = "vxlan0"
	}
	vmPodCIDR := opts.VMPodCIDR
	if vmPodCIDR == "" {
		vmPodCIDR = a.PodCIDR
	}
	hostVeth, nsVeth := vethNames(ns)
	wgHostName := "ppw" + shortHash(ns)
	hostIP, nsIP := transportIPs(ns)
	fwmark := routeMark(ns)
	listenPort := opts.WireGuardPort
	if listenPort == 0 {
		listenPort = a.Tunnel.WireGuardListenPort
	}

	t := &Tunnel{
		Allocation: a,
		Namespace:  ns,
		ipCommand:  ipCommand,
		wgCommand:  wgCommand,
		iptables:   iptablesCommand,
		sysctl:     sysctlCommand,
		underlay:   underlay,
		listenPort: listenPort,
		wgIface:    wgIface,
		wgHostName: wgHostName,
		vxlanIface: vxlanIface,
		hostVeth:   hostVeth,
		nsVeth:     nsVeth,
		hostIP:     hostIP,
		nsIP:       nsIP,
		vmPodCIDR:  vmPodCIDR,
		routes:     opts.AdditionalRoutes,
		endpoint:   opts.AllowEndpoint,
		masquerade: opts.Masquerade,
		fwmark:     fwmark,
		table:      fwmark,
		rulePrio:   fwmark,
		created:    false,
	}
	if opts.Setup {
		if err := t.setup(ctx); err != nil {
			return nil, err
		}
		t.created = true
	}

	return t, nil
}

func (t *Tunnel) Run(ctx context.Context, name string, args ...string) error {
	_, err := t.Output(ctx, name, args...)

	return err
}

func (t *Tunnel) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"netns", "exec", t.Namespace, name}, args...)
	cmd := exec.CommandContext(ctx, t.ipCommand, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("run in playpen netns: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return out, nil
}

func (t *Tunnel) Close(ctx context.Context) error {
	if !t.created {
		return nil
	}

	return t.cleanup(ctx)
}

func (t *Tunnel) cleanup(ctx context.Context) error {
	_ = run(ctx, t.iptables, "-t", "nat", "-D", "POSTROUTING", "-s", t.nsIP+"/32", "-j", "MASQUERADE")
	_ = run(ctx, t.ipCommand, "link", "delete", t.hostVeth)
	cmd := exec.CommandContext(ctx, t.ipCommand, "netns", "delete", t.Namespace)
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such file") {
		return fmt.Errorf("delete playpen netns: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func (t *Tunnel) setup(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("playpen tunnel setup requires root or CAP_NET_ADMIN")
	}
	if t.Allocation.RequiresEndpoint && !t.allowEndpoint() {
		return fmt.Errorf("allocation requires playpen endpoint fallback; direct local tunnel setup is not available")
	}

	if t.Allocation.Tunnel.WireGuardPrivateKey == "" {
		return fmt.Errorf("allocation does not include a WireGuard private key; allocate through pkg/playpen or provide ClientWireGuardPublicKey and configure the tunnel manually")
	}
	if len(t.Allocation.GatewayPeers) == 0 {
		return fmt.Errorf("allocation did not return any gateway peers")
	}

	steps := [][]string{
		{"netns", "add", t.Namespace},
		{"link", "add", t.hostVeth, "type", "veth", "peer", "name", t.nsVeth},
		{"link", "set", t.nsVeth, "netns", t.Namespace},
		{"addr", "add", t.hostIP + "/30", "dev", t.hostVeth},
		{"link", "set", t.hostVeth, "up"},
		{"-n", t.Namespace, "addr", "add", t.nsIP + "/30", "dev", t.nsVeth},
		{"-n", t.Namespace, "link", "set", "lo", "up"},
		{"-n", t.Namespace, "link", "set", t.nsVeth, "up"},
		{"-n", t.Namespace, "route", "replace", "default", "via", t.hostIP, "dev", t.nsVeth},
		{"link", "add", t.wgHostName, "type", "wireguard"},
		{"link", "set", t.wgHostName, "netns", t.Namespace},
		{"-n", t.Namespace, "link", "set", t.wgHostName, "name", t.wgIface},
		{"-n", t.Namespace, "addr", "add", t.underlay, "dev", t.wgIface},
		{"-n", t.Namespace, "link", "set", t.wgIface, "up"},
		{"-n", t.Namespace, "link", "add", t.vxlanIface, "type", "vxlan", "external", "dstport", fmt.Sprint(t.Allocation.Tunnel.VXLANPort), "nolearning"},
		{"-n", t.Namespace, "addr", "add", firstNonEmpty(t.Allocation.Lease.Router, "169.254.254.1") + "/32", "dev", t.vxlanIface},
		{"-n", t.Namespace, "link", "set", t.vxlanIface, "up"},
	}
	for _, args := range steps {
		if err := run(ctx, t.ipCommand, args...); err != nil {
			_ = t.cleanup(context.Background())
			return err
		}
	}
	if err := run(ctx, t.sysctl, "-w", "net.ipv4.ip_forward=1"); err != nil {
		_ = t.cleanup(context.Background())
		return err
	}
	if err := run(ctx, t.iptables, "-t", "nat", "-A", "POSTROUTING", "-s", t.nsIP+"/32", "-j", "MASQUERADE"); err != nil {
		_ = t.cleanup(context.Background())
		return err
	}
	t.created = true

	wgArgs := []string{"netns", "exec", t.Namespace, t.wgCommand, "set", t.wgIface, "private-key", "/dev/stdin", "fwmark", strconv.Itoa(t.fwmark)}
	if t.listenPort > 0 {
		wgArgs = append(wgArgs, "listen-port", strconv.Itoa(t.listenPort))
	}
	if err := runWithInput(ctx, t.ipCommand, t.Allocation.Tunnel.WireGuardPrivateKey, wgArgs...); err != nil {
		_ = t.cleanup(context.Background())
		return err
	}
	if err := t.ensureWireGuardEndpointRule(ctx); err != nil {
		_ = t.cleanup(context.Background())
		return err
	}

	for _, peer := range t.Allocation.GatewayPeers {
		if peer.WireGuardPublicKey == "" || len(peer.Endpoints) == 0 {
			continue
		}
		endpointIP := endpointHost(peer.Endpoints[0])
		if err := t.addWireGuardEndpointRoute(ctx, endpointIP); err != nil {
			_ = t.cleanup(context.Background())
			return err
		}
		allowed := append([]string{}, peer.PodCIDRs...)
		allowed = append(allowed, peer.RoutedCIDRs...)
		for _, ip := range peer.InternalIPs {
			if cidr := hostCIDR(ip); cidr != "" {
				allowed = append(allowed, cidr)
			}
		}
		if len(allowed) == 0 {
			return fmt.Errorf("gateway peer %q has no safe WireGuard allowed IPs", peer.Name)
		}
		if err := run(ctx, t.ipCommand, "netns", "exec", t.Namespace, t.wgCommand, "set", t.wgIface, "peer", peer.WireGuardPublicKey, "endpoint", peer.Endpoints[0], "allowed-ips", strings.Join(allowed, ","), "persistent-keepalive", "15"); err != nil {
			_ = t.cleanup(context.Background())
			return err
		}
		for _, cidr := range allowed {
			if cidr == "0.0.0.0/0" || cidr == "::/0" {
				continue
			}
			if err := run(ctx, t.ipCommand, "-n", t.Namespace, "route", "replace", cidr, "dev", t.wgIface); err != nil {
				_ = t.cleanup(context.Background())
				return err
			}
		}
		for _, internalIP := range peer.InternalIPs {
			if internalIP == "" {
				continue
			}
			if err := t.addVXLANRoutes(ctx, internalIP, append([]string{t.vmPodCIDR}, t.routes...)); err != nil {
				_ = t.cleanup(context.Background())
				return err
			}

			break
		}
	}
	if t.masquerade && t.vmPodCIDR != "" {
		if err := run(ctx, t.ipCommand, "netns", "exec", t.Namespace, t.iptables, "-t", "nat", "-A", "POSTROUTING", "-d", t.vmPodCIDR, "-j", "MASQUERADE"); err != nil {
			_ = t.cleanup(context.Background())
			return err
		}
	}

	return nil
}

func (t *Tunnel) allowEndpoint() bool {
	return t.endpoint
}

func (t *Tunnel) addVXLANRoutes(ctx context.Context, remoteUnderlay string, routes []string) error {
	for _, route := range routes {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}
		if err := run(ctx, t.ipCommand, "-n", t.Namespace, "route", "replace", route, "encap", "ip", "id", fmt.Sprint(t.Allocation.Tunnel.VXLANVNI), "dst", remoteUnderlay, "dev", t.vxlanIface); err != nil {
			return err
		}
	}

	return nil
}

func (t *Tunnel) addWireGuardEndpointRoute(ctx context.Context, endpointIP string) error {
	if endpointIP == "" {
		return nil
	}
	cidr := hostCIDR(endpointIP)
	if cidr == "" {
		return nil
	}
	if err := run(ctx, t.ipCommand, "-n", t.Namespace, "route", "replace", cidr, "via", t.hostIP, "dev", t.nsVeth, "table", strconv.Itoa(t.table)); err != nil {
		return err
	}

	return nil
}

func (t *Tunnel) ensureWireGuardEndpointRule(ctx context.Context) error {
	_ = run(ctx, t.ipCommand, "-n", t.Namespace, "rule", "del", "priority", strconv.Itoa(t.rulePrio))

	return run(ctx, t.ipCommand, "-n", t.Namespace, "rule", "add", "fwmark", strconv.Itoa(t.fwmark), "table", strconv.Itoa(t.table), "priority", strconv.Itoa(t.rulePrio))
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}

func runWithInput(ctx context.Context, name, input string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}

func clientUnderlayIP(seed string) string {
	if seed == "" {
		seed = "playpen"
	}
	sum := shortHashUint(seed)

	return fmt.Sprintf("169.254.%d.%d", 100+(sum%100), 1+(sum/100)%200)
}

func hostCIDR(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return parsed.String() + "/32"
	}

	return parsed.String() + "/128"
}

func subnetMask(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil || ipnet == nil {
		return ""
	}
	mask := net.IP(ipnet.Mask).To4()
	if mask == nil {
		return ""
	}

	return mask.String()
}

func endpointHost(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err == nil {
		return strings.Trim(host, "[]")
	}

	if i := strings.LastIndex(endpoint, ":"); i > 0 && strings.Count(endpoint, ":") == 1 {
		return endpoint[:i]
	}

	return endpoint
}

func vethNames(seed string) (string, string) {
	suffix := shortHash(seed)

	return "pph" + suffix, "ppn" + suffix
}

func transportIPs(seed string) (string, string) {
	sum := shortHashUint(seed)
	third := 200 + (sum % 40)
	fourth := (sum / 40 % 60) * 4
	if fourth == 0 {
		fourth = 4
	}

	return fmt.Sprintf("169.254.%d.%d", third, fourth+1), fmt.Sprintf("169.254.%d.%d", third, fourth+2)
}

func shortHash(seed string) string {
	sum := uint32(2166136261)
	for _, b := range []byte(seed) {
		sum ^= uint32(b)
		sum *= 16777619
	}

	return fmt.Sprintf("%08x", sum)
}

func routeMark(seed string) int {
	return 50000 + int(shortHashUint(seed)%10000)
}

func shortHashUint(seed string) uint32 {
	sum := uint32(2166136261)
	for _, b := range []byte(seed) {
		sum ^= uint32(b)
		sum *= 16777619
	}

	return sum
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
