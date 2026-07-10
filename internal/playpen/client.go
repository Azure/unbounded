// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultClientNamespace    = "playpen-client"
	defaultClientBridgeName   = "br-playpen"
	defaultClientVXLANName    = "vxlan0"
	defaultClientUnderlay     = "underlay0"
	defaultClientWireGuard    = "wg0"
	defaultClientBridgeCIDR   = "192.168.100.1/24"
	defaultClientTunnelMTU    = 1230
	defaultPlaypenNamespace   = "unbounded-kube"
	defaultPlaypenSelector    = "app.kubernetes.io/name=playpen"
	clientShutdownExitSignal  = syscall.SIGTERM
	clientClaimReleaseTimeout = 10 * time.Second
	clientCommandStopTimeout  = 5 * time.Second
)

// ClientConfig describes a client-side playpen VXLAN endpoint.
type ClientConfig struct {
	Namespace     string
	EndpointCIDR  string
	GatewayIP     string
	RemoteIP      string
	BridgeCIDR    string
	BridgeName    string
	VXLANName     string
	UnderlayName  string
	WireGuardName string
	NodeIP        string
	NodeCIDR      string
	Site          string
	GatewayPool   string
	WireGuardPort int
	VXLANVNI      int
	VXLANPort     int
	MTU           int
	Command       []string
	IPBinary      string
	WGBinary      string
	PodNamespace  string
	PodSelector   string
	Kubeconfig    string
	KubeContext   string
	KubeClient    kubernetes.Interface
	DynamicClient dynamic.Interface
	ClaimOutput   io.Writer
}

// DefaultClientConfig returns defaults that are safe because all fixed link
// names and sockets live inside the client network namespace.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Namespace:     defaultClientNamespace,
		BridgeCIDR:    defaultClientBridgeCIDR,
		BridgeName:    defaultClientBridgeName,
		VXLANName:     defaultClientVXLANName,
		UnderlayName:  defaultClientUnderlay,
		WireGuardName: defaultClientWireGuard,
		WireGuardPort: 51820,
		VXLANVNI:      defaultVXLANVNI,
		VXLANPort:     defaultVXLANPort,
		IPBinary:      "ip",
		WGBinary:      "wg",
		PodNamespace:  defaultPlaypenNamespace,
		PodSelector:   defaultPlaypenSelector,
		ClaimOutput:   io.Discard,
	}
}

// NormalizeClientConfig applies defaults and validates a client configuration.
func NormalizeClientConfig(cfg ClientConfig) (ClientConfig, error) {
	defaults := DefaultClientConfig()
	if cfg.Namespace == "" {
		cfg.Namespace = defaults.Namespace
	}

	if cfg.BridgeCIDR == "" {
		cfg.BridgeCIDR = defaults.BridgeCIDR
	}

	if cfg.BridgeName == "" {
		cfg.BridgeName = defaults.BridgeName
	}

	if cfg.VXLANName == "" {
		cfg.VXLANName = defaults.VXLANName
	}

	if cfg.UnderlayName == "" {
		cfg.UnderlayName = defaults.UnderlayName
	}

	if cfg.WireGuardName == "" {
		cfg.WireGuardName = defaults.WireGuardName
	}

	if cfg.WireGuardPort == 0 {
		cfg.WireGuardPort = defaults.WireGuardPort
	}

	if cfg.VXLANVNI == 0 {
		cfg.VXLANVNI = defaults.VXLANVNI
	}

	if cfg.VXLANPort == 0 {
		cfg.VXLANPort = defaults.VXLANPort
	}

	if cfg.MTU == 0 {
		if clientWireGuardEnabled(cfg) {
			cfg.MTU = defaultClientTunnelMTU
		} else {
			cfg.MTU = defaultMTU
		}
	}

	if cfg.IPBinary == "" {
		cfg.IPBinary = defaults.IPBinary
	}

	if cfg.WGBinary == "" {
		cfg.WGBinary = defaults.WGBinary
	}

	if cfg.ClaimOutput == nil {
		cfg.ClaimOutput = defaults.ClaimOutput
	}

	var errs []error
	if !validNamespaceName(cfg.Namespace) {
		errs = append(errs, fmt.Errorf("namespace %q must contain only letters, numbers, '.', '_' or '-'", cfg.Namespace))
	}

	endpointIP, endpointNet, err := net.ParseCIDR(cfg.EndpointCIDR)
	if err != nil {
		errs = append(errs, fmt.Errorf("endpoint-cidr must be an IP prefix: %q", cfg.EndpointCIDR))
	}

	gatewayIP := net.ParseIP(cfg.GatewayIP)
	if gatewayIP == nil {
		errs = append(errs, fmt.Errorf("gateway-ip must be an IP address: %q", cfg.GatewayIP))
	}

	remoteIP := net.ParseIP(cfg.RemoteIP)
	if remoteIP == nil {
		errs = append(errs, fmt.Errorf("remote must be an IP address: %q", cfg.RemoteIP))
	}

	if _, _, err := net.ParseCIDR(cfg.BridgeCIDR); err != nil {
		errs = append(errs, fmt.Errorf("bridge-cidr must be an IP prefix: %q", cfg.BridgeCIDR))
	}

	if endpointNet != nil && gatewayIP != nil {
		if !endpointNet.Contains(gatewayIP) {
			errs = append(errs, fmt.Errorf("gateway-ip %s is outside endpoint network %s", gatewayIP, endpointNet))
		}

		if endpointIP.Equal(gatewayIP) {
			errs = append(errs, errors.New("endpoint-cidr address and gateway-ip must differ"))
		}
	}

	if endpointIP != nil && remoteIP != nil && (endpointIP.To4() == nil) != (remoteIP.To4() == nil) {
		errs = append(errs, errors.New("endpoint-cidr and remote must use the same IP family"))
	}

	tunneled := clientWireGuardEnabled(cfg)
	if tunneled {
		if strings.TrimSpace(cfg.Site) == "" {
			errs = append(errs, errors.New("site is required when WireGuard tunneling is enabled"))
		}

		if strings.TrimSpace(cfg.GatewayPool) == "" {
			errs = append(errs, errors.New("gateway-pool is required when WireGuard tunneling is enabled"))
		}

		nodeInternalIP := net.ParseIP(cfg.NodeIP)
		if nodeInternalIP == nil {
			errs = append(errs, fmt.Errorf("node-ip must be an IP address when WireGuard tunneling is enabled: %q", cfg.NodeIP))
		} else if endpointIP != nil {
			if (endpointIP.To4() == nil) != (nodeInternalIP.To4() == nil) {
				errs = append(errs, errors.New("endpoint-cidr and node-ip must use the same IP family"))
			}

			if endpointNet != nil && endpointNet.Contains(nodeInternalIP) {
				errs = append(errs, errors.New("node-ip must be outside the endpoint-cidr underlay network"))
			}
		}

		if cfg.MTU > defaultClientTunnelMTU {
			errs = append(errs, fmt.Errorf("mtu must not exceed %d when WireGuard tunneling is enabled: %d", defaultClientTunnelMTU, cfg.MTU))
		}

		nodeIP, nodeNet, nodeErr := net.ParseCIDR(cfg.NodeCIDR)
		if nodeErr != nil {
			errs = append(errs, fmt.Errorf("node-cidr must be an IP prefix when WireGuard tunneling is enabled: %q", cfg.NodeCIDR))
		} else {
			if nodeIP.Equal(nodeNet.IP) {
				nodeIP = firstUsableIP(nodeNet)
			}

			if endpointIP != nil && (endpointIP.To4() == nil) != (nodeIP.To4() == nil) {
				errs = append(errs, errors.New("endpoint-cidr and node-cidr must use the same IP family"))
			}

			if endpointNet != nil && cidrsOverlap(endpointNet, nodeNet) {
				errs = append(errs, errors.New("endpoint-cidr and node-cidr must not overlap"))
			}
		}

		if cfg.WireGuardPort < 1 || cfg.WireGuardPort > 65535 {
			errs = append(errs, fmt.Errorf("wireguard-port must be between 1 and 65535: %d", cfg.WireGuardPort))
		}
	}

	if cfg.VXLANVNI < 1 || cfg.VXLANVNI > maxVXLANVNI {
		errs = append(errs, fmt.Errorf("vxlan-vni must be between 1 and %d: %d", maxVXLANVNI, cfg.VXLANVNI))
	}

	if cfg.VXLANPort < 1 || cfg.VXLANPort > 65535 {
		errs = append(errs, fmt.Errorf("vxlan-port must be between 1 and 65535: %d", cfg.VXLANPort))
	}

	if cfg.MTU < 576 || cfg.MTU > 65535 {
		errs = append(errs, fmt.Errorf("mtu must be between 576 and 65535: %d", cfg.MTU))
	}

	for label, name := range map[string]string{
		"bridge": cfg.BridgeName, "vxlan": cfg.VXLANName, "underlay": cfg.UnderlayName,
	} {
		if err := validateInterfaceName(label, name); err != nil {
			errs = append(errs, err)
		}
	}

	if tunneled {
		if err := validateInterfaceName("wireguard", cfg.WireGuardName); err != nil {
			errs = append(errs, err)
		}

		if cfg.WireGuardName == cfg.BridgeName || cfg.WireGuardName == cfg.VXLANName || cfg.WireGuardName == cfg.UnderlayName {
			errs = append(errs, errors.New("wireguard, bridge, vxlan, and underlay interface names must be distinct"))
		}
	}

	if cfg.BridgeName == cfg.VXLANName || cfg.BridgeName == cfg.UnderlayName || cfg.VXLANName == cfg.UnderlayName {
		errs = append(errs, errors.New("bridge, vxlan, and underlay interface names must be distinct"))
	}

	if len(cfg.Command) == 0 {
		errs = append(errs, errors.New("a command to run in the client namespace is required"))
	}

	return cfg, errors.Join(errs...)
}

// RunClient creates the isolated endpoint, runs cfg.Command in it, and removes
// the namespace when the command exits or ctx is canceled.
func RunClient(ctx context.Context, cfg ClientConfig) (retErr error) {
	claim, err := claimPlaypenPodForClient(ctx, cfg)
	if err != nil {
		return err
	}

	if claim != nil {
		cfg.RemoteIP = claim.remoteIP

		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clientClaimReleaseTimeout)
			defer cancel()

			if err := claim.release(releaseCtx); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("release playpen pod %s/%s: %w", claim.namespace, claim.name, err))
			}
		}()
	}

	cfg, err = NormalizeClientConfig(cfg)
	if err != nil {
		return err
	}

	if claim != nil {
		if err := writePlaypenClaim(cfg.ClaimOutput, claim); err != nil {
			return fmt.Errorf("write playpen claim result: %w", err)
		}
	}

	endpointIP, endpointNet, err := net.ParseCIDR(cfg.EndpointCIDR)
	if err != nil {
		return fmt.Errorf("parse normalized endpoint-cidr: %w", err)
	}

	remoteIP := net.ParseIP(cfg.RemoteIP)
	if remoteIP == nil {
		return errors.New("parse normalized remote IP")
	}

	prefixLength, _ := endpointNet.Mask.Size()
	gatewayCIDR := fmt.Sprintf("%s/%d", cfg.GatewayIP, prefixLength)

	runner := clientCommandRunner{binary: cfg.IPBinary}

	var tunnel *clientWireGuardTunnel
	if clientWireGuardEnabled(cfg) {
		tunnel, err = prepareClientWireGuard(ctx, cfg, endpointIP, remoteIP)
		if err != nil {
			return err
		}

		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clientClaimReleaseTimeout)
			defer cancel()

			if err := tunnel.removeNode(cleanupCtx); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}()
	}

	hostLink := clientHostLinkName(cfg.Namespace)

	peerLink := clientPeerLinkName(cfg.Namespace)
	if err := runner.runContext(ctx, "netns", "add", cfg.Namespace); err != nil {
		return fmt.Errorf("create network namespace %s: %w", cfg.Namespace, err)
	}

	defer func() {
		if err := runner.run("netns", "delete", cfg.Namespace); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("delete network namespace %s: %w", cfg.Namespace, err))
		}
	}()

	if err := runner.run("link", "add", hostLink, "type", "veth", "peer", "name", peerLink); err != nil {
		return fmt.Errorf("create client veth: %w", err)
	}

	defer func() {
		if err := runner.run("link", "delete", hostLink); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("delete client veth %s: %w", hostLink, err))
		}
	}()

	underlayMTU := cfg.MTU + 50
	if tunnel != nil {
		underlayMTU += 80
	}

	commands := [][]string{
		{"link", "set", peerLink, "netns", cfg.Namespace, "name", cfg.UnderlayName},
		{"addr", "add", gatewayCIDR, "dev", hostLink},
		{"link", "set", hostLink, "mtu", strconv.Itoa(underlayMTU)},
		{"link", "set", hostLink, "up"},
		{"-n", cfg.Namespace, "link", "set", "lo", "up"},
		{"-n", cfg.Namespace, "addr", "add", cfg.EndpointCIDR, "dev", cfg.UnderlayName},
		{"-n", cfg.Namespace, "link", "set", cfg.UnderlayName, "mtu", strconv.Itoa(underlayMTU)},
		{"-n", cfg.Namespace, "link", "set", cfg.UnderlayName, "up"},
		{"-n", cfg.Namespace, "route", "add", "default", "via", cfg.GatewayIP},
		{"-n", cfg.Namespace, "link", "add", cfg.BridgeName, "type", "bridge"},
		{"-n", cfg.Namespace, "addr", "add", cfg.BridgeCIDR, "dev", cfg.BridgeName},
		{"-n", cfg.Namespace, "link", "set", cfg.BridgeName, "mtu", strconv.Itoa(cfg.MTU)},
		{"-n", cfg.Namespace, "link", "set", cfg.BridgeName, "up"},
	}
	for _, args := range commands {
		if err := runner.runContext(ctx, args...); err != nil {
			return fmt.Errorf("configure client namespace: %w", err)
		}
	}

	vxlanLocalIP := endpointIP

	vxlanDevice := cfg.UnderlayName
	if tunnel != nil {
		if err := tunnel.configureNamespace(ctx, runner, cfg, remoteIP); err != nil {
			return err
		}

		vxlanLocalIP = tunnel.address
		vxlanDevice = cfg.WireGuardName
	}

	vxlanCommands := [][]string{
		{"-n", cfg.Namespace, "link", "add", cfg.VXLANName, "type", "vxlan", "id", strconv.Itoa(cfg.VXLANVNI), "local", vxlanLocalIP.String(), "remote", remoteIP.String(), "dstport", strconv.Itoa(cfg.VXLANPort), "dev", vxlanDevice, "nolearning"},
		{"-n", cfg.Namespace, "link", "set", cfg.VXLANName, "master", cfg.BridgeName},
		{"-n", cfg.Namespace, "link", "set", cfg.VXLANName, "mtu", strconv.Itoa(cfg.MTU)},
		{"-n", cfg.Namespace, "link", "set", cfg.VXLANName, "up"},
	}
	for _, args := range vxlanCommands {
		if err := runner.runContext(ctx, args...); err != nil {
			return fmt.Errorf("configure client VXLAN: %w", err)
		}
	}

	args := append([]string{"netns", "exec", cfg.Namespace}, cfg.Command...)

	cmd := exec.Command(cfg.IPBinary, args...)
	if claim != nil {
		cmd.Env = claim.environment(os.Environ())
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start client command: %w", err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("client command exited: %w", err)
		}

		return nil
	case <-ctx.Done():
		if err := syscall.Kill(-cmd.Process.Pid, clientShutdownExitSignal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop client command: %w", err)
		}

		select {
		case <-done:
		case <-time.After(clientCommandStopTimeout):
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill client command: %w", err)
			}

			<-done
		}

		return nil
	}
}

func clientWireGuardEnabled(cfg ClientConfig) bool {
	return strings.TrimSpace(cfg.NodeIP) != "" || strings.TrimSpace(cfg.NodeCIDR) != "" || strings.TrimSpace(cfg.Site) != "" || strings.TrimSpace(cfg.GatewayPool) != ""
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

type clientCommandRunner struct{ binary string }

func (r clientCommandRunner) run(args ...string) error {
	return r.runContext(context.Background(), args...)
}

func (r clientCommandRunner) runContext(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, r.binary, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", r.binary, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return nil
}

func clientHostLinkName(namespace string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(namespace))

	return fmt.Sprintf("pp%08xh", hash.Sum32())
}

func clientPeerLinkName(namespace string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(namespace))

	return fmt.Sprintf("pp%08xp", hash.Sum32())
}

func validNamespaceName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}

	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}

		return false
	}

	return true
}
