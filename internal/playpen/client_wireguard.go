// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

const (
	clientNodeOwnerAnnotation = "playpen.unbounded-cloud.io/client-id"
	clientNodeTypeLabel       = "playpen.unbounded-cloud.io/external-client"
	clientWireGuardPubKey     = "net.unbounded-cloud.io/wg-pubkey"
	clientTunnelReadyTimeout  = 45 * time.Second
	clientWireGuardMTU        = defaultClientTunnelMTU + 50
)

var (
	siteResource        = schema.GroupVersionResource{Group: unboundednetv1alpha1.GroupName, Version: "v1alpha1", Resource: "sites"}
	gatewayPoolResource = schema.GroupVersionResource{Group: unboundednetv1alpha1.GroupName, Version: "v1alpha1", Resource: "gatewaypools"}
	assignmentResource  = schema.GroupVersionResource{Group: unboundednetv1alpha1.GroupName, Version: "v1alpha1", Resource: "sitegatewaypoolassignments"}
)

type clientWireGuardTunnel struct {
	address          net.IP
	privateKey       wgtypes.Key
	gatewayPublicKey string
	gatewayEndpoint  string
	nodes            kubernetes.Interface
	nodeName         string
	nodeUID          types.UID
}

func prepareClientWireGuard(ctx context.Context, cfg ClientConfig, endpointIP, remoteIP net.IP) (*clientWireGuardTunnel, error) {
	kubeClient, dynamicClient, err := clientKubeClients(cfg)
	if err != nil {
		return nil, err
	}

	site, err := getClientNetResource[unboundednetv1alpha1.Site](ctx, dynamicClient, siteResource, cfg.Site)
	if err != nil {
		return nil, fmt.Errorf("get Site %s: %w", cfg.Site, err)
	}

	nodeIP := net.ParseIP(cfg.NodeIP)
	if !siteContainsIP(site.Spec.NodeCidrs, nodeIP) {
		return nil, fmt.Errorf("node IP %s is outside nodeCidrs for Site %s", nodeIP, cfg.Site)
	}

	address, nodeNet, err := clientNodeAddress(cfg.NodeCIDR)
	if err != nil {
		return nil, err
	}

	if err := validateClientNodeCIDR(ctx, kubeClient, *site, nodeNet, remoteIP, cfg.BridgeCIDR); err != nil {
		return nil, err
	}

	pool, err := getClientNetResource[unboundednetv1alpha1.GatewayPool](ctx, dynamicClient, gatewayPoolResource, cfg.GatewayPool)
	if err != nil {
		return nil, fmt.Errorf("get GatewayPool %s: %w", cfg.GatewayPool, err)
	}

	if pool.Spec.Type != "External" {
		return nil, fmt.Errorf("GatewayPool %s must have type External", cfg.GatewayPool)
	}

	if err := validateClientNodeIsNotGateway(ctx, dynamicClient, cfg, nodeIP, nodeNet.String()); err != nil {
		return nil, err
	}

	assigned, err := clientSiteAssignedToPool(ctx, dynamicClient, cfg.Site, cfg.GatewayPool)
	if err != nil {
		return nil, fmt.Errorf("list SiteGatewayPoolAssignments: %w", err)
	}

	if !assigned {
		return nil, fmt.Errorf("site %s is not enabled in a SiteGatewayPoolAssignment for GatewayPool %s", cfg.Site, cfg.GatewayPool)
	}

	gateway, err := selectClientGateway(pool.Status.Nodes, endpointIP)
	if err != nil {
		return nil, fmt.Errorf("select gateway from GatewayPool %s: %w", cfg.GatewayPool, err)
	}

	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate WireGuard private key: %w", err)
	}

	if _, err := wgtypes.ParseKey(gateway.WireGuardPublicKey); err != nil {
		return nil, fmt.Errorf("gateway %s has an invalid WireGuard public key: %w", gateway.Name, err)
	}

	clientID := string(uuid.NewUUID())
	node := clientNode(cfg, nodeIP, nodeNet.String(), privateKey.PublicKey().String(), clientID)

	created, err := kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("temporary Node %s already exists; use a unique --namespace", node.Name)
	}

	if err != nil {
		return nil, fmt.Errorf("create temporary Node %s: %w", node.Name, err)
	}

	tunnel := &clientWireGuardTunnel{
		address:          address,
		privateKey:       privateKey,
		gatewayPublicKey: gateway.WireGuardPublicKey,
		gatewayEndpoint:  net.JoinHostPort(firstIPForFamily(gateway.ExternalIPs, endpointIP).String(), strconv.Itoa(cfg.WireGuardPort)),
		nodes:            kubeClient,
		nodeName:         created.Name,
		nodeUID:          created.UID,
	}

	created.Status.Addresses = node.Status.Addresses
	if _, err := kubeClient.CoreV1().Nodes().UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clientClaimReleaseTimeout)
		defer cancel()

		cleanupErr := tunnel.removeNode(cleanupCtx)

		return nil, errors.Join(fmt.Errorf("publish temporary Node %s status: %w", node.Name, err), cleanupErr)
	}

	return tunnel, nil
}

func clientSiteAssignedToPool(ctx context.Context, client dynamic.Interface, site, pool string) (bool, error) {
	objects, err := client.Resource(assignmentResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}

	data, err := objects.MarshalJSON()
	if err != nil {
		return false, err
	}

	assignments := &unboundednetv1alpha1.SiteGatewayPoolAssignmentList{}
	if err := json.Unmarshal(data, assignments); err != nil {
		return false, err
	}

	for _, assignment := range assignments.Items {
		if unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) && containsString(assignment.Spec.Sites, site) && containsString(assignment.Spec.GatewayPools, pool) {
			return true, nil
		}
	}

	return false, nil
}

func validateClientNodeCIDR(ctx context.Context, client kubernetes.Interface, site unboundednetv1alpha1.Site, nodeNet *net.IPNet, remoteIP net.IP, bridgeCIDR string) error {
	wantMask := clientNodeMask(site.Spec.PodCidrAssignments, nodeNet)

	ones, _ := nodeNet.Mask.Size()
	if wantMask == 0 || ones != wantMask {
		return fmt.Errorf("node-cidr %s must be an aligned per-node block from Site %s podCidrAssignments", nodeNet, site.Name)
	}

	_, bridgeNet, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return fmt.Errorf("parse normalized bridge CIDR: %w", err)
	}

	if nodeNet.Contains(remoteIP) || (bridgeNet != nil && cidrsOverlap(nodeNet, bridgeNet)) {
		return fmt.Errorf("node-cidr %s overlaps the playpen remote or bridge network", nodeNet)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Nodes for node-cidr validation: %w", err)
	}

	for i := range nodes.Items {
		podCIDRs := nodes.Items[i].Spec.PodCIDRs
		if len(podCIDRs) == 0 && nodes.Items[i].Spec.PodCIDR != "" {
			podCIDRs = []string{nodes.Items[i].Spec.PodCIDR}
		}

		for _, cidr := range podCIDRs {
			_, existing, parseErr := net.ParseCIDR(cidr)
			if parseErr == nil && cidrsOverlap(nodeNet, existing) {
				return fmt.Errorf("node-cidr %s overlaps Node %s pod CIDR %s", nodeNet, nodes.Items[i].Name, existing)
			}
		}
	}

	return nil
}

func clientNodeMask(assignments []unboundednetv1alpha1.PodCidrAssignment, nodeNet *net.IPNet) int {
	wantIPv4 := nodeNet.IP.To4() != nil
	ones, _ := nodeNet.Mask.Size()

	for _, assignment := range assignments {
		for _, block := range assignment.CidrBlocks {
			_, pool, err := net.ParseCIDR(block)
			if err != nil || (pool.IP.To4() != nil) != wantIPv4 || !pool.Contains(nodeNet.IP) || !pool.Contains(lastIP(nodeNet)) {
				continue
			}

			mask := 0

			if assignment.NodeBlockSizes != nil {
				if wantIPv4 {
					mask = assignment.NodeBlockSizes.IPv4
				} else {
					mask = assignment.NodeBlockSizes.IPv6
				}
			}

			if mask == 0 {
				if wantIPv4 {
					mask = 24
				} else {
					poolOnes, _ := pool.Mask.Size()
					mask = min(poolOnes+16, 128)
				}
			}

			if ones == mask {
				return mask
			}
		}
	}

	return 0
}

func validateClientNodeIsNotGateway(ctx context.Context, client dynamic.Interface, cfg ClientConfig, internalIP net.IP, nodeCIDR string) error {
	objects, err := client.Resource(gatewayPoolResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list GatewayPools: %w", err)
	}

	data, err := objects.MarshalJSON()
	if err != nil {
		return err
	}

	pools := &unboundednetv1alpha1.GatewayPoolList{}
	if err := json.Unmarshal(data, pools); err != nil {
		return err
	}

	nodeLabels := labels.Set(clientNode(cfg, internalIP, nodeCIDR, "", "").Labels)
	for _, pool := range pools.Items {
		if labels.SelectorFromSet(pool.Spec.NodeSelector).Matches(nodeLabels) {
			return fmt.Errorf("temporary Node labels match GatewayPool %s; gateway selectors must exclude external playpen clients", pool.Name)
		}
	}

	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}

	return false
}

func clientKubeClients(cfg ClientConfig) (kubernetes.Interface, dynamic.Interface, error) {
	coreClient := cfg.KubeClient

	dynamicClient := cfg.DynamicClient
	if coreClient != nil && dynamicClient != nil {
		return coreClient, dynamicClient, nil
	}

	restConfig, err := playpenRESTConfig(cfg.Kubeconfig, cfg.KubeContext)
	if err != nil {
		return nil, nil, err
	}

	if coreClient == nil {
		coreClient, err = kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("create Kubernetes client: %w", err)
		}
	}

	if dynamicClient == nil {
		dynamicClient, err = dynamic.NewForConfig(restConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("create dynamic Kubernetes client: %w", err)
		}
	}

	return coreClient, dynamicClient, nil
}

func getClientNetResource[T any](ctx context.Context, client dynamic.Interface, resource schema.GroupVersionResource, name string) (*T, error) {
	object, err := client.Resource(resource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	data, err := object.MarshalJSON()
	if err != nil {
		return nil, err
	}

	result := new(T)
	if err := json.Unmarshal(data, result); err != nil {
		return nil, err
	}

	return result, nil
}

func selectClientGateway(gateways []unboundednetv1alpha1.GatewayNodeInfo, familyIP net.IP) (unboundednetv1alpha1.GatewayNodeInfo, error) {
	sort.Slice(gateways, func(i, j int) bool { return gateways[i].Name < gateways[j].Name })

	for _, gateway := range gateways {
		if gateway.WireGuardPublicKey != "" && firstIPForFamily(gateway.ExternalIPs, familyIP) != nil {
			return gateway, nil
		}
	}

	return unboundednetv1alpha1.GatewayNodeInfo{}, errors.New("no gateway has a public IP and WireGuard key for the client IP family")
}

func siteContainsIP(cidrs []string, ip net.IP) bool {
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			return true
		}
	}

	return false
}

func firstIPForFamily(values []string, familyIP net.IP) net.IP {
	for _, value := range values {
		ip := net.ParseIP(value)
		if ip != nil && (ip.To4() != nil) == (familyIP.To4() != nil) {
			return ip
		}
	}

	return nil
}

func clientNodeAddress(cidr string) (net.IP, *net.IPNet, error) {
	address, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse node-cidr: %w", err)
	}

	ones, bits := network.Mask.Size()
	if ones == bits {
		address = network.IP
	} else if address.Equal(network.IP) {
		address = firstUsableIP(network)
	}

	if !network.Contains(address) || isIPv4Broadcast(address, network) {
		return nil, nil, fmt.Errorf("node-cidr has no usable address: %s", cidr)
	}

	return address, network, nil
}

func lastIP(network *net.IPNet) net.IP {
	ip := append(net.IP(nil), network.IP...)
	for i := range ip {
		ip[i] |= ^network.Mask[i]
	}

	return ip
}

func isIPv4Broadcast(ip net.IP, network *net.IPNet) bool {
	ones, bits := network.Mask.Size()
	return bits == 32 && ones < 31 && ip.Equal(lastIP(network))
}

func firstUsableIP(network *net.IPNet) net.IP {
	ip := append(net.IP(nil), network.IP...)
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}

	return ip
}

func clientNode(cfg ClientConfig, internalIP net.IP, nodeCIDR, publicKey, clientID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: clientNodeName(cfg.Namespace),
			Labels: map[string]string{
				unboundednetv1alpha1.SiteLabelKey: cfg.Site,
				clientNodeTypeLabel:               "true",
			},
			Annotations: map[string]string{
				clientNodeOwnerAnnotation: clientID,
				clientWireGuardPubKey:     publicKey,
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDR:       nodeCIDR,
			PodCIDRs:      []string{nodeCIDR},
			Unschedulable: true,
			Taints: []corev1.Taint{{
				Key:    clientNodeTypeLabel,
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: internalIP.String()}}},
	}
}

func clientNodeName(namespace string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(namespace))

	return fmt.Sprintf("playpen-client-%08x", hash.Sum32())
}

func (t *clientWireGuardTunnel) configureNamespace(ctx context.Context, runner clientCommandRunner, cfg ClientConfig, remoteIP net.IP) (retErr error) {
	remoteCIDR := remoteIP.String() + "/32"

	addressCIDR := t.address.String() + "/32"
	if remoteIP.To4() == nil {
		remoteCIDR = remoteIP.String() + "/128"
		addressCIDR = t.address.String() + "/128"
	}

	hostWireGuardName := clientWireGuardHostLinkName(cfg.Namespace)
	if err := runner.runContext(ctx, "link", "add", hostWireGuardName, "type", "wireguard"); err != nil {
		return fmt.Errorf("create WireGuard interface: %w", err)
	}

	moved := false

	defer func() {
		if !moved {
			if err := runner.run("link", "delete", hostWireGuardName); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("delete WireGuard interface %s: %w", hostWireGuardName, err))
			}
		}
	}()

	args := []string{"set", hostWireGuardName, "private-key", "/dev/stdin", "peer", t.gatewayPublicKey, "endpoint", t.gatewayEndpoint, "allowed-ips", remoteCIDR, "persistent-keepalive", "5"}
	cmd := exec.CommandContext(ctx, cfg.WGBinary, args...)

	cmd.Stdin = strings.NewReader(t.privateKey.String() + "\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("configure WireGuard interface: %w: %s", err, strings.TrimSpace(string(output)))
	}

	if err := runner.runContext(ctx, "link", "set", hostWireGuardName, "netns", cfg.Namespace, "name", cfg.WireGuardName); err != nil {
		return fmt.Errorf("move WireGuard interface into client namespace: %w", err)
	}

	moved = true

	commands := [][]string{
		{"-n", cfg.Namespace, "addr", "add", addressCIDR, "dev", cfg.WireGuardName},
		{"-n", cfg.Namespace, "link", "set", cfg.WireGuardName, "mtu", strconv.Itoa(clientWireGuardMTU)},
		{"-n", cfg.Namespace, "link", "set", cfg.WireGuardName, "up"},
		{"-n", cfg.Namespace, "route", "add", remoteCIDR, "dev", cfg.WireGuardName},
	}
	for _, args := range commands {
		if err := runner.runContext(ctx, args...); err != nil {
			return fmt.Errorf("configure WireGuard route: %w", err)
		}
	}

	deadline := time.Now().Add(clientTunnelReadyTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		showArgs := []string{"netns", "exec", cfg.Namespace, cfg.WGBinary, "show", cfg.WireGuardName, "latest-handshakes"}

		output, err := exec.CommandContext(ctx, cfg.IPBinary, showArgs...).Output()
		if err == nil {
			fields := bytes.Fields(output)
			if len(fields) >= 2 && string(fields[1]) != "0" {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return fmt.Errorf("WireGuard gateway %s did not complete a handshake within %s", t.gatewayEndpoint, clientTunnelReadyTimeout)
}

func clientWireGuardHostLinkName(namespace string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(namespace))

	return fmt.Sprintf("pp%08xw", hash.Sum32())
}

func (t *clientWireGuardTunnel) removeNode(ctx context.Context) error {
	uid := t.nodeUID

	err := t.nodes.CoreV1().Nodes().Delete(ctx, t.nodeName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("delete temporary Node %s: %w", t.nodeName, err)
	}

	return nil
}
