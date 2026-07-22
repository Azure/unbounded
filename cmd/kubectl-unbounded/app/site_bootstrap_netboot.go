// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/net/nodeagent"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	defaultBootstrapNetbootTimeout = 30 * time.Minute
	defaultBootstrapPollInterval   = time.Second
)

type siteBootstrapNetbootHandler struct {
	site                   string
	machine                string
	interfaceName          string
	address                string
	gatewayExternalAddress string
	endpointName           string
	httpPort               int
	kubeconfigPath         string
	namespace              string
	metalmanBinary         string
	timeout                time.Duration
	routedCIDRs            []string
	resources              client.Client
	kubeClient             kubernetes.Interface
	restConfig             *rest.Config
	pollInterval           time.Duration
	dependencies           bootstrapNetbootDependencies
}

type bootstrapNetbootDependencies struct {
	resolveBinary     func() (string, error)
	preflightNetwork  func() error
	localPort         func() (int, error)
	portForward       func(context.Context, int) (*bootstrapPortForward, error)
	edgeToken         func(context.Context) (*bootstrapEdgeToken, error)
	startEdge         func(string, []string) (bootstrapEdgeProcess, error)
	dialEdge          func(context.Context, string) error
	preflightGateway  func() error
	gatewayRuntimeDir func() (string, error)
	startGateway      func(context.Context, nodeagent.ExternalGatewayOptions) (bootstrapEdgeProcess, error)
}

type bootstrapNetbootState struct {
	originalEndpointRef string
}

type bootstrapPortForwardAttempt interface {
	Done() <-chan error
	Stop()
}

type bootstrapPortForwardStarter func(
	ctx context.Context,
	podName string,
	localPort int,
	remotePort int,
) (bootstrapPortForwardAttempt, error)

type bootstrapPortForward struct {
	url    string
	cancel context.CancelFunc
	done   chan struct{}
}

type bootstrapEdgeToken struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type bootstrapEdgeProcess interface {
	Done() <-chan struct{}
	Err() error
	Stop(ctx context.Context) error
}

type commandBootstrapEdgeProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	err      error
}

type embeddedBootstrapGatewayProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func (p *commandBootstrapEdgeProcess) Done() <-chan struct{} {
	return p.done
}

func (p *commandBootstrapEdgeProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.err
}

func (p *commandBootstrapEdgeProcess) Stop(ctx context.Context) error {
	var signalErr error

	p.stopOnce.Do(func() {
		signalErr = p.cmd.Process.Signal(syscall.SIGTERM)
	})

	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return fmt.Errorf("stop Metalman edge: %w", signalErr)
	}

	select {
	case <-p.done:
		err := p.Err()
		if err != nil && !isSignalExit(err) {
			return fmt.Errorf("wait for Metalman edge: %w", err)
		}

		return nil
	case <-ctx.Done():
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill Metalman edge: %w", err)
		}

		<-p.done

		return ctx.Err()
	}
}

func (p *embeddedBootstrapGatewayProcess) Done() <-chan struct{} { return p.done }

func (p *embeddedBootstrapGatewayProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.err
}

func (p *embeddedBootstrapGatewayProcess) Stop(ctx context.Context) error {
	p.once.Do(p.cancel)

	select {
	case <-p.done:
		return p.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *bootstrapEdgeToken) Path() string {
	return t.path
}

func (t *bootstrapEdgeToken) Close() error {
	var err error

	t.once.Do(func() {
		t.cancel()
		<-t.done
		err = os.RemoveAll(filepath.Dir(t.path))
	})

	return err
}

func (f *bootstrapPortForward) URL() string {
	return f.url
}

func (f *bootstrapPortForward) Close() error {
	f.cancel()
	<-f.done

	return nil
}

func siteBootstrapNetbootCommand(handler *siteBootstrapNetbootHandler) *cobra.Command {
	if handler == nil {
		handler = &siteBootstrapNetbootHandler{}
	}

	cmd := &cobra.Command{
		Use:   "bootstrap-netboot SITE",
		Short: "Temporarily serve netboot from this machine until the first Site node is Ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handler.site = args[0]

			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.machine, "machine", "", "Machine whose Node readiness completes bootstrap")
	cmd.Flags().StringVar(&handler.interfaceName, "interface", "", "Provisioning network interface for DHCP and TFTP")
	cmd.Flags().StringVar(&handler.address, "address", "", "Address on the provisioning interface advertised to clients")
	cmd.Flags().StringVar(&handler.endpointName, "endpoint-name", "", "Ephemeral NetbootEndpoint name (generated when omitted)")
	cmd.Flags().IntVar(&handler.httpPort, "http-port", 8880, "Local HTTP artifact port")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.namespace, "namespace", unbounded.SystemNamespace(), "Namespace containing Metalman workloads")
	cmd.Flags().StringVar(&handler.metalmanBinary, "metalman-binary", "", "Path to the metalman binary")
	cmd.Flags().StringVar(&handler.gatewayExternalAddress, "gateway-external-address", "", "WireGuard address reachable by remote gateway peers (defaults to --address)")
	cmd.Flags().DurationVar(&handler.timeout, "timeout", defaultBootstrapNetbootTimeout, "Maximum time to wait for the designated Node to become Ready")
	cmd.Flags().StringSliceVar(&handler.routedCIDRs, "routed-cidr", nil, "CIDR routed through an ephemeral external gateway (repeatable)")

	if err := cmd.MarkFlagRequired("machine"); err != nil {
		panic(fmt.Sprintf("mark machine flag required: %v", err))
	}

	if err := cmd.MarkFlagRequired("interface"); err != nil {
		panic(fmt.Sprintf("mark interface flag required: %v", err))
	}

	if err := cmd.MarkFlagRequired("address"); err != nil {
		panic(fmt.Sprintf("mark address flag required: %v", err))
	}

	return cmd
}

func (h *siteBootstrapNetbootHandler) execute(ctx context.Context) (retErr error) {
	if h.timeout <= 0 {
		h.timeout = defaultBootstrapNetbootTimeout
	}

	if h.endpointName == "" {
		h.endpointName = "bootstrap-" + h.machine
	}

	if h.gatewayExternalAddress == "" {
		h.gatewayExternalAddress = h.address
	}

	if err := h.initializeDependencies(); err != nil {
		return err
	}

	binary, err := h.dependencies.resolveBinary()
	if err != nil {
		return err
	}

	if err := h.dependencies.preflightNetwork(); err != nil {
		return err
	}

	var gatewayRuntimeDir string

	if len(h.routedCIDRs) > 0 {
		if err := h.dependencies.preflightGateway(); err != nil {
			return err
		}

		gatewayRuntimeDir, err = h.dependencies.gatewayRuntimeDir()
		if err != nil {
			return fmt.Errorf("create external gateway runtime directory: %w", err)
		}

		defer func() { retErr = errors.Join(retErr, os.RemoveAll(gatewayRuntimeDir)) }()
	}

	if err := h.initializeClients(); err != nil {
		return err
	}

	h.initializeRuntimeDependencies()

	runCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	state, err := h.prepareClusterResources(runCtx)
	if err != nil {
		return err
	}

	resourcesPrepared := true

	var (
		forward         *bootstrapPortForward
		token           *bootstrapEdgeToken
		process         bootstrapEdgeProcess
		gatewayProcess  bootstrapEdgeProcess
		gatewayPrepared bool
	)

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanupCancel()

		if resourcesPrepared {
			retErr = errors.Join(retErr, h.restoreClusterResources(cleanupCtx, state))
		}

		if process != nil {
			retErr = errors.Join(retErr, process.Stop(cleanupCtx))
		}

		if gatewayProcess != nil {
			retErr = errors.Join(retErr, gatewayProcess.Stop(cleanupCtx))
		}

		if forward != nil {
			retErr = errors.Join(retErr, forward.Close())
		}

		if token != nil {
			retErr = errors.Join(retErr, token.Close())
		}

		if gatewayPrepared {
			retErr = errors.Join(retErr, h.cleanupGatewayResources(cleanupCtx))
		}
	}()

	if len(h.routedCIDRs) > 0 {
		if err := h.prepareGatewayResources(runCtx); err != nil {
			return err
		}

		gatewayPrepared = true

		gatewayProcess, err = h.dependencies.startGateway(runCtx, nodeagent.ExternalGatewayOptions{
			NodeName: h.endpointName, RuntimeDir: gatewayRuntimeDir, RESTConfig: h.restConfig,
		})
		if err != nil {
			return fmt.Errorf("start external gateway dataplane: %w", err)
		}

		if err := h.waitForGatewayReady(runCtx, gatewayProcess); err != nil {
			return err
		}
	}

	if err := h.waitForMetalman(runCtx); err != nil {
		return err
	}

	localPort, err := h.dependencies.localPort()
	if err != nil {
		return fmt.Errorf("select local port for Metalman server: %w", err)
	}

	forward, err = h.dependencies.portForward(runCtx, localPort)
	if err != nil {
		return err
	}

	token, err = h.dependencies.edgeToken(runCtx)
	if err != nil {
		return err
	}

	process, err = h.dependencies.startEdge(binary, h.edgeArguments(forward.URL(), token.Path()))
	if err != nil {
		return err
	}

	if err := h.waitForEdgeReady(runCtx, process, h.dependencies.dialEdge); err != nil {
		return err
	}

	if err := h.claimEndpoint(runCtx, "kubectl-unbounded/"+h.endpointName); err != nil {
		return err
	}

	var machine v1alpha3.Machine
	if err := h.resources.Get(runCtx, client.ObjectKey{Name: h.machine}, &machine); err != nil {
		return fmt.Errorf("get designated Machine %s: %w", h.machine, err)
	}

	return h.waitForNodeReadyAndProcesses(runCtx, designatedNodeName(&machine), process, gatewayProcess)
}

func (h *siteBootstrapNetbootHandler) initializeDependencies() error {
	if h.dependencies.resolveBinary == nil {
		h.dependencies.resolveBinary = func() (string, error) {
			return h.resolveMetalmanBinary(exec.LookPath)
		}
	}

	if h.dependencies.preflightNetwork == nil {
		h.dependencies.preflightNetwork = h.validateProvisioningInterface
	}

	if h.dependencies.localPort == nil {
		h.dependencies.localPort = availableLoopbackPort
	}

	if h.dependencies.startEdge == nil {
		h.dependencies.startEdge = func(binary string, args []string) (bootstrapEdgeProcess, error) {
			return startBootstrapEdgeProcess(binary, args, os.Stdout, os.Stderr)
		}
	}

	if h.dependencies.dialEdge == nil {
		h.dependencies.dialEdge = dialBootstrapEdge
	}

	if h.dependencies.preflightGateway == nil {
		h.dependencies.preflightGateway = h.validateExternalGateway
	}

	if h.dependencies.gatewayRuntimeDir == nil {
		h.dependencies.gatewayRuntimeDir = func() (string, error) {
			return os.MkdirTemp("/run", "unbounded-netboot-")
		}
	}

	if h.dependencies.startGateway == nil {
		h.dependencies.startGateway = startEmbeddedBootstrapGateway
	}

	return nil
}

func (h *siteBootstrapNetbootHandler) initializeClients() error {
	if h.resources != nil && h.kubeClient != nil && h.restConfig != nil {
		return nil
	}

	kubeClient, config, err := kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfigPath))
	if err != nil {
		return fmt.Errorf("create Kubernetes client for netboot bootstrap: %w", err)
	}

	resources, err := client.New(config, client.Options{Scheme: buildScheme()})
	if err != nil {
		return fmt.Errorf("create resource client for netboot bootstrap: %w", err)
	}

	h.kubeClient = kubeClient
	h.resources = resources
	h.restConfig = config

	return nil
}

func (h *siteBootstrapNetbootHandler) initializeRuntimeDependencies() {
	if h.dependencies.portForward == nil {
		starter := newSPDYBootstrapPortForwardStarter(h.kubeClient, h.restConfig, h.namespace)
		h.dependencies.portForward = func(ctx context.Context, localPort int) (*bootstrapPortForward, error) {
			return newBootstrapPortForward(
				ctx,
				h.kubeClient,
				h.namespace,
				"metalman-server-"+h.site,
				localPort,
				8880,
				starter,
				h.pollInterval,
			)
		}
	}

	if h.dependencies.edgeToken == nil {
		h.dependencies.edgeToken = func(ctx context.Context) (*bootstrapEdgeToken, error) {
			return newBootstrapEdgeToken(ctx, h.kubeClient, h.namespace, "", 0)
		}
	}
}

func (h *siteBootstrapNetbootHandler) validateProvisioningInterface() error {
	interfaceInfo, err := net.InterfaceByName(h.interfaceName)
	if err != nil {
		return fmt.Errorf("find provisioning interface %s: %w", h.interfaceName, err)
	}

	wanted := net.ParseIP(h.address)
	if wanted == nil || wanted.To4() == nil {
		return fmt.Errorf("provisioning address %q must be an IPv4 address", h.address)
	}

	addresses, err := interfaceInfo.Addrs()
	if err != nil {
		return fmt.Errorf("list addresses on provisioning interface %s: %w", h.interfaceName, err)
	}

	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.Equal(wanted) {
			return nil
		}
	}

	return fmt.Errorf("provisioning interface %s does not own address %s", h.interfaceName, h.address)
}

func (h *siteBootstrapNetbootHandler) validateExternalGateway() error {
	if err := h.validateRoutedCIDRs(); err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("external gateway dataplane requires root privileges")
	}

	if ip := net.ParseIP(h.gatewayExternalAddress); ip == nil || ip.To4() == nil {
		return fmt.Errorf("gateway external address %q must be an IPv4 address", h.gatewayExternalAddress)
	}

	return nil
}

func (h *siteBootstrapNetbootHandler) validateRoutedCIDRs() error {
	for _, routedCIDR := range h.routedCIDRs {
		if _, _, err := net.ParseCIDR(routedCIDR); err != nil {
			return fmt.Errorf("invalid routed CIDR %q: %w", routedCIDR, err)
		}
	}

	return nil
}

func availableLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close() //nolint:errcheck // Best-effort cleanup after reserving the port.

	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("loopback listener returned address type %T", listener.Addr())
	}

	return tcpAddress.Port, nil
}

func dialBootstrapEdge(ctx context.Context, address string) error {
	dialer := net.Dialer{}

	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}

	return connection.Close()
}

func (h *siteBootstrapNetbootHandler) prepareClusterResources(ctx context.Context) (bootstrapNetbootState, error) {
	var site v1alpha3.Site
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.site}, &site); err != nil {
		return bootstrapNetbootState{}, fmt.Errorf("get Site %s: %w", h.site, err)
	}

	enabled := true

	if site.Spec.Components.Metalman == nil {
		site.Spec.Components.Metalman = &v1alpha3.MetalmanComponentSpec{}
	}

	site.Spec.Components.Metalman.Enabled = &enabled

	if err := h.resources.Update(ctx, &site); err != nil {
		return bootstrapNetbootState{}, fmt.Errorf("enable Metalman for Site %s: %w", h.site, err)
	}

	var machine v1alpha3.Machine
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.machine}, &machine); err != nil {
		return bootstrapNetbootState{}, fmt.Errorf("get Machine %s: %w", h.machine, err)
	}

	if machine.Labels[v1alpha3.MachineSiteLabelKey] != h.site {
		return bootstrapNetbootState{}, fmt.Errorf("machine %s does not belong to Site %s", h.machine, h.site)
	}

	netboot := machine.Spec.Netboot()
	if netboot == nil {
		return bootstrapNetbootState{}, fmt.Errorf("machine %s has no netboot configuration", h.machine)
	}

	state := bootstrapNetbootState{originalEndpointRef: netboot.EndpointRef}
	externalURL := (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(h.address, strconv.Itoa(h.httpPort)),
	}).String()
	endpoint := &v1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: h.endpointName},
		Spec: v1alpha3.NetbootEndpointSpec{
			SiteRef:     h.site,
			Type:        v1alpha3.NetbootEndpointTypeExternalL2,
			ExternalURL: externalURL,
			TLS: v1alpha3.NetbootEndpointTLS{
				Trust: v1alpha3.NetbootEndpointTrustTrustedLAN,
				Mode:  v1alpha3.NetbootEndpointTLSDisabled,
			},
		},
	}

	if err := h.resources.Create(ctx, endpoint); err != nil {
		return bootstrapNetbootState{}, fmt.Errorf("create NetbootEndpoint %s: %w", h.endpointName, err)
	}

	netboot.EndpointRef = h.endpointName
	if err := h.resources.Update(ctx, &machine); err != nil {
		if deleteErr := h.resources.Delete(ctx, endpoint); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return bootstrapNetbootState{}, errors.Join(
				fmt.Errorf("select bootstrap endpoint for Machine %s: %w", h.machine, err),
				fmt.Errorf("delete NetbootEndpoint %s after Machine update failure: %w", h.endpointName, deleteErr),
			)
		}

		return bootstrapNetbootState{}, fmt.Errorf("select bootstrap endpoint for Machine %s: %w", h.machine, err)
	}

	return state, nil
}

func (h *siteBootstrapNetbootHandler) restoreClusterResources(ctx context.Context, state bootstrapNetbootState) error {
	var machine v1alpha3.Machine
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.machine}, &machine); err != nil {
		return fmt.Errorf("get Machine %s for cleanup: %w", h.machine, err)
	}

	if netboot := machine.Spec.Netboot(); netboot != nil && netboot.EndpointRef == h.endpointName {
		netboot.EndpointRef = state.originalEndpointRef

		if err := h.resources.Update(ctx, &machine); err != nil {
			return fmt.Errorf("restore Machine %s endpoint: %w", h.machine, err)
		}
	}

	endpoint := &v1alpha3.NetbootEndpoint{ObjectMeta: metav1.ObjectMeta{Name: h.endpointName}}
	if err := h.resources.Delete(ctx, endpoint); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete NetbootEndpoint %s: %w", h.endpointName, err)
	}

	return nil
}

func (h *siteBootstrapNetbootHandler) prepareGatewayResources(ctx context.Context) (retErr error) {
	if err := h.validateRoutedCIDRs(); err != nil {
		return err
	}

	protocol := netv1alpha1.TunnelProtocolWireGuard
	enabled := true
	selector := map[string]string{"net.unbounded-cloud.io/bootstrap-gateway": h.endpointName}

	pool := &netv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: h.endpointName},
		Spec: netv1alpha1.GatewayPoolSpec{
			Type:           "External",
			NodeSelector:   selector,
			RoutedCidrs:    append([]string(nil), h.routedCIDRs...),
			TunnelProtocol: &protocol,
		},
	}
	if err := h.resources.Create(ctx, pool); err != nil {
		return fmt.Errorf("create bootstrap GatewayPool %s: %w", h.endpointName, err)
	}

	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, h.cleanupGatewayResources(context.WithoutCancel(ctx)))
		}
	}()

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: h.endpointName},
		Spec: netv1alpha1.SiteGatewayPoolAssignmentSpec{
			Enabled:        &enabled,
			Sites:          []string{h.site},
			GatewayPools:   []string{h.endpointName},
			TunnelProtocol: &protocol,
		},
	}
	if err := h.resources.Create(ctx, assignment); err != nil {
		return fmt.Errorf("create bootstrap SiteGatewayPoolAssignment %s: %w", h.endpointName, err)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: h.endpointName,
			Labels: map[string]string{
				"net.unbounded-cloud.io/bootstrap-gateway": h.endpointName,
				"net.unbounded-cloud.io/external-node":     "true",
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints: []corev1.Taint{{
				Key: "net.unbounded-cloud.io/gateway-node", Value: "true", Effect: corev1.TaintEffectNoSchedule,
			}},
		},
	}

	createdNode, err := h.kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create bootstrap gateway Node %s: %w", h.endpointName, err)
	}

	createdNode.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: h.address},
		{Type: corev1.NodeExternalIP, Address: h.gatewayExternalAddress},
	}
	if _, err := h.kubeClient.CoreV1().Nodes().UpdateStatus(ctx, createdNode, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("set bootstrap gateway Node %s addresses: %w", h.endpointName, err)
	}

	return nil
}

func (h *siteBootstrapNetbootHandler) cleanupGatewayResources(ctx context.Context) error {
	assignment := &netv1alpha1.SiteGatewayPoolAssignment{ObjectMeta: metav1.ObjectMeta{Name: h.endpointName}}

	assignmentErr := h.resources.Delete(ctx, assignment)
	if apierrors.IsNotFound(assignmentErr) {
		assignmentErr = nil
	}

	if assignmentErr != nil {
		assignmentErr = fmt.Errorf("delete bootstrap SiteGatewayPoolAssignment %s: %w", h.endpointName, assignmentErr)
	}

	nodeErr := h.kubeClient.CoreV1().Nodes().Delete(ctx, h.endpointName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(nodeErr) {
		nodeErr = nil
	}

	if nodeErr != nil {
		nodeErr = fmt.Errorf("delete bootstrap gateway Node %s: %w", h.endpointName, nodeErr)
	}

	pool := &netv1alpha1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: h.endpointName}}

	poolErr := h.resources.Delete(ctx, pool)
	if apierrors.IsNotFound(poolErr) {
		poolErr = nil
	}

	if poolErr != nil {
		poolErr = fmt.Errorf("delete bootstrap GatewayPool %s: %w", h.endpointName, poolErr)
	}

	return errors.Join(assignmentErr, nodeErr, poolErr)
}

func (h *siteBootstrapNetbootHandler) metalmanDeploymentsReady(ctx context.Context) (bool, error) {
	for _, name := range []string{"metalman-controller-" + h.site, "metalman-server-" + h.site} {
		deployment, err := h.kubeClient.AppsV1().Deployments(h.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("get Deployment %s/%s: %w", h.namespace, name, err)
		}

		if !deploymentRolloutComplete(deployment) {
			return false, nil
		}
	}

	return true, nil
}

func (h *siteBootstrapNetbootHandler) waitForMetalman(ctx context.Context) error {
	interval := h.pollInterval
	if interval <= 0 {
		interval = defaultBootstrapPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		ready, err := h.metalmanDeploymentsReady(ctx)
		if err != nil {
			return err
		}

		if ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Metalman controller and server: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (h *siteBootstrapNetbootHandler) edgeArguments(backendURL, tokenFile string) []string {
	return []string{
		"edge",
		"--backend-url=" + backendURL,
		"--endpoint=" + h.endpointName,
		"--bind-address=" + h.address,
		"--http-port=" + strconv.Itoa(h.httpPort),
		"--dhcp-enabled",
		"--dhcp-interface=" + h.interfaceName,
		"--dhcp-server-ip=" + h.address,
		"--edge-token-file=" + tokenFile,
		"--tftp-enabled",
		"--tftp-bind-address=" + h.address,
	}
}

func (h *siteBootstrapNetbootHandler) resolveMetalmanBinary(lookPath func(string) (string, error)) (string, error) {
	if h.metalmanBinary != "" {
		info, err := os.Stat(h.metalmanBinary)
		if err != nil {
			return "", fmt.Errorf("metalman binary %q is not accessible: %w", h.metalmanBinary, err)
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("metalman binary %q is not executable", h.metalmanBinary)
		}

		return h.metalmanBinary, nil
	}

	path, err := lookPath("metalman")
	if err != nil {
		return "", fmt.Errorf("find metalman executable: %w; install it or set --metalman-binary", err)
	}

	return path, nil
}

func (h *siteBootstrapNetbootHandler) waitForEdgeReady(
	ctx context.Context,
	process bootstrapEdgeProcess,
	dial func(context.Context, string) error,
) error {
	interval := h.pollInterval
	if interval <= 0 {
		interval = defaultBootstrapPollInterval
	}

	address := net.JoinHostPort(h.address, strconv.Itoa(h.httpPort))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := dial(ctx, address); err == nil {
			return nil
		}

		select {
		case <-process.Done():
			if err := process.Err(); err != nil {
				return fmt.Errorf("metalman edge exited before becoming ready: %w", err)
			}

			return errors.New("metalman edge exited before becoming ready")
		case <-ctx.Done():
			return fmt.Errorf("wait for Metalman edge listener: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func startBootstrapEdgeProcess(binary string, args []string, stdout, stderr io.Writer) (bootstrapEdgeProcess, error) {
	cmd := exec.Command(binary, args...)
	cmd.Stdout = stdout

	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Metalman edge: %w", err)
	}

	process := &commandBootstrapEdgeProcess{cmd: cmd, done: make(chan struct{})}

	go func() {
		err := cmd.Wait()

		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()

	return process, nil
}

func startEmbeddedBootstrapGateway(
	ctx context.Context,
	options nodeagent.ExternalGatewayOptions,
) (bootstrapEdgeProcess, error) {
	runCtx, cancel := context.WithCancel(ctx)
	process := &embeddedBootstrapGatewayProcess{cancel: cancel, done: make(chan struct{})}

	go func() {
		err := nodeagent.RunExternalGateway(runCtx, options)

		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()

	return process, nil
}

func isSignalExit(err error) bool {
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ProcessState == nil {
		return false
	}

	status, ok := exitError.Sys().(syscall.WaitStatus)

	return ok && status.Signaled()
}

func (h *siteBootstrapNetbootHandler) claimEndpoint(ctx context.Context, identity string) error {
	var endpoint v1alpha3.NetbootEndpoint
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &endpoint); err != nil {
		return fmt.Errorf("get NetbootEndpoint %s: %w", h.endpointName, err)
	}

	now := metav1.Now()
	endpoint.Status.ObservedGeneration = endpoint.Generation
	endpoint.Status.Claim = &v1alpha3.NetbootEndpointClaim{
		HolderIdentity: identity,
		RenewedAt:      now,
	}
	metaSetStatusCondition(&endpoint.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: endpoint.Generation,
		Reason:             "ExternalEdgeReady",
		Message:            "administrator bootstrap edge is ready",
		LastTransitionTime: now,
	})

	if err := h.resources.Status().Update(ctx, &endpoint); err != nil {
		return fmt.Errorf("claim NetbootEndpoint %s: %w", h.endpointName, err)
	}

	return nil
}

func (h *siteBootstrapNetbootHandler) waitForNodeReady(ctx context.Context, nodeName string) error {
	interval := h.pollInterval
	if interval <= 0 {
		interval = defaultBootstrapPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		node, err := h.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get designated Node %s: %w", nodeName, err)
		}

		if err == nil && nodeReady(node) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for designated Node %s to become Ready: %w", nodeName, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (h *siteBootstrapNetbootHandler) waitForNodeReadyAndProcesses(
	ctx context.Context,
	nodeName string,
	edge bootstrapEdgeProcess,
	gateway bootstrapEdgeProcess,
) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ready := make(chan error, 1)

	go func() { ready <- h.waitForNodeReady(waitCtx, nodeName) }()

	var gatewayDone <-chan struct{}
	if gateway != nil {
		gatewayDone = gateway.Done()
	}

	select {
	case err := <-ready:
		return err
	case <-edge.Done():
		return processExitError("metalman edge", nodeName, edge.Err())
	case <-gatewayDone:
		return processExitError("external gateway", nodeName, gateway.Err())
	case <-ctx.Done():
		return fmt.Errorf("wait for designated Node %s while bootstrap dataplanes are running: %w", nodeName, ctx.Err())
	}
}

func processExitError(name, nodeName string, err error) error {
	if err != nil {
		return fmt.Errorf("%s exited before designated Node %s became Ready: %w", name, nodeName, err)
	}

	return fmt.Errorf("%s exited before designated Node %s became Ready", name, nodeName)
}

func (h *siteBootstrapNetbootHandler) waitForGatewayReady(ctx context.Context, process bootstrapEdgeProcess) error {
	interval := h.pollInterval
	if interval <= 0 {
		interval = defaultBootstrapPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		ready, err := h.gatewayReady(ctx)
		if err != nil {
			return err
		}

		if ready {
			return nil
		}

		select {
		case <-process.Done():
			if err := process.Err(); err != nil {
				return fmt.Errorf("external gateway exited before becoming ready: %w", err)
			}

			return fmt.Errorf("external gateway exited before becoming ready")
		case <-ctx.Done():
			return fmt.Errorf("wait for external gateway readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (h *siteBootstrapNetbootHandler) gatewayReady(ctx context.Context) (bool, error) {
	node, err := h.kubeClient.CoreV1().Nodes().Get(ctx, h.endpointName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get bootstrap gateway Node %s: %w", h.endpointName, err)
	}

	if node.Annotations["net.unbounded-cloud.io/wg-pubkey"] == "" {
		return false, nil
	}

	var pool netv1alpha1.GatewayPool
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &pool); err != nil {
		return false, fmt.Errorf("get bootstrap GatewayPool %s: %w", h.endpointName, err)
	}

	if pool.Status.NodeCount != 1 || !containsString(pool.Status.ConnectedSites, h.site) {
		return false, nil
	}

	matchedNode := false

	for _, poolNode := range pool.Status.Nodes {
		if poolNode.Name == h.endpointName && poolNode.WireGuardPublicKey != "" {
			matchedNode = true
			break
		}
	}

	if !matchedNode {
		return false, nil
	}

	var gatewayNode netv1alpha1.GatewayPoolNode
	if err := h.resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &gatewayNode); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get bootstrap GatewayPoolNode %s: %w", h.endpointName, err)
	}

	return !gatewayNode.Status.LastUpdated.IsZero() && len(gatewayNode.Status.Routes) > 0, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}

	return false
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func designatedNodeName(machine *v1alpha3.Machine) string {
	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name != "" {
		return machine.Spec.Kubernetes.NodeRef.Name
	}

	return machine.Name
}

func metaSetStatusCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	for i := range *conditions {
		if (*conditions)[i].Type == condition.Type {
			(*conditions)[i] = condition

			return
		}
	}

	*conditions = append(*conditions, condition)
}

func newBootstrapPortForward(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	deploymentName string,
	localPort int,
	remotePort int,
	start bootstrapPortForwardStarter,
	retryInterval time.Duration,
) (*bootstrapPortForward, error) {
	if retryInterval <= 0 {
		retryInterval = defaultBootstrapPollInterval
	}

	forwardCtx, cancel := context.WithCancel(ctx)

	podName, err := readyDeploymentPod(forwardCtx, kubeClient, namespace, deploymentName, "")
	if err != nil {
		cancel()

		return nil, err
	}

	attempt, err := start(forwardCtx, podName, localPort, remotePort)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("start port-forward to Pod %s: %w", podName, err)
	}

	forward := &bootstrapPortForward{
		url:    "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)),
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(forward.done)

		current := attempt

		for {
			select {
			case <-forwardCtx.Done():
				current.Stop()
				<-current.Done()

				return
			case <-current.Done():
			}

			for {
				select {
				case <-forwardCtx.Done():
					current.Stop()

					return
				case <-time.After(retryInterval):
				}

				podName, err = readyDeploymentPod(forwardCtx, kubeClient, namespace, deploymentName, podName)
				if err != nil {
					continue
				}

				current, err = start(forwardCtx, podName, localPort, remotePort)
				if err == nil {
					break
				}
			}
		}
	}()

	return forward, nil
}

func readyDeploymentPod(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	deploymentName string,
	excludedPod string,
) (string, error) {
	deployment, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get Deployment %s/%s for port-forward: %w", namespace, deploymentName, err)
	}

	selector := metav1.FormatLabelSelector(deployment.Spec.Selector)

	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("list Pods for Deployment %s/%s: %w", namespace, deploymentName, err)
	}

	ready := make([]string, 0, len(pods.Items))
	excludedReady := false

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && podReady(pod) {
			if pod.Name == excludedPod {
				excludedReady = true

				continue
			}

			ready = append(ready, pod.Name)
		}
	}

	if len(ready) == 0 && excludedReady {
		ready = append(ready, excludedPod)
	}

	if len(ready) == 0 {
		return "", fmt.Errorf("deployment %s/%s has no Ready Pods", namespace, deploymentName)
	}

	sort.Strings(ready)

	return ready[0], nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

type spdyBootstrapPortForwardAttempt struct {
	stop chan struct{}
	done chan error
	once sync.Once
}

func (a *spdyBootstrapPortForwardAttempt) Done() <-chan error {
	return a.done
}

func (a *spdyBootstrapPortForwardAttempt) Stop() {
	a.once.Do(func() { close(a.stop) })
}

func newSPDYBootstrapPortForwardStarter(
	kubeClient kubernetes.Interface,
	config *rest.Config,
	namespace string,
) bootstrapPortForwardStarter {
	return func(
		ctx context.Context,
		podName string,
		localPort int,
		remotePort int,
	) (bootstrapPortForwardAttempt, error) {
		targetURL := kubeClient.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(namespace).
			Name(podName).
			SubResource("portforward").
			URL()

		transport, upgrader, err := spdy.RoundTripperFor(config)
		if err != nil {
			return nil, fmt.Errorf("create SPDY transport: %w", err)
		}

		stop := make(chan struct{})
		ready := make(chan struct{})
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, targetURL)

		forwarder, err := portforward.NewOnAddresses(
			dialer,
			[]string{"127.0.0.1"},
			[]string{fmt.Sprintf("%d:%d", localPort, remotePort)},
			stop,
			ready,
			io.Discard,
			io.Discard,
		)
		if err != nil {
			return nil, fmt.Errorf("create port-forward: %w", err)
		}

		attempt := &spdyBootstrapPortForwardAttempt{stop: stop, done: make(chan error, 1)}

		go func() { attempt.done <- forwarder.ForwardPorts() }()

		select {
		case <-ctx.Done():
			attempt.Stop()

			return nil, ctx.Err()
		case err := <-attempt.done:
			attempt.Stop()

			return nil, fmt.Errorf("port-forward exited before becoming ready: %w", err)
		case <-ready:
			return attempt, nil
		case <-time.After(30 * time.Second):
			attempt.Stop()

			return nil, fmt.Errorf("port-forward to Pod %s timed out", podName)
		}
	}
}

func newBootstrapEdgeToken(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	tempRoot string,
	refreshInterval time.Duration,
) (*bootstrapEdgeToken, error) {
	directory, err := os.MkdirTemp(tempRoot, "unbounded-netboot-")
	if err != nil {
		return nil, fmt.Errorf("create edge token directory: %w", err)
	}

	path := filepath.Join(directory, "edge-token")
	if err := refreshBootstrapEdgeToken(ctx, kubeClient, namespace, path); err != nil {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("remove edge token directory: %w", removeErr))
		}

		return nil, err
	}

	if refreshInterval <= 0 {
		refreshInterval = 20 * time.Minute
	}

	tokenCtx, cancel := context.WithCancel(ctx)
	credential := &bootstrapEdgeToken{
		path:   path,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(credential.done)

		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-tokenCtx.Done():
				return
			case <-ticker.C:
				if err := refreshBootstrapEdgeToken(tokenCtx, kubeClient, namespace, path); err != nil && tokenCtx.Err() == nil {
					slog.WarnContext(tokenCtx, "refreshing Metalman edge token failed", "err", err)
				}
			}
		}
	}()

	return credential, nil
}

func refreshBootstrapEdgeToken(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	path string,
) error {
	expirationSeconds := int64(time.Hour / time.Second)

	response, err := kubeClient.CoreV1().ServiceAccounts(namespace).CreateToken(
		ctx,
		"metalman-edge",
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"metalman-edge"},
			ExpirationSeconds: &expirationSeconds,
		}},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("request metalman-edge token: %w", err)
	}

	if response.Status.Token == "" {
		return fmt.Errorf("request metalman-edge token: API returned an empty token")
	}

	temporaryPath := path + ".new"
	if err := os.WriteFile(temporaryPath, []byte(response.Status.Token), 0o600); err != nil {
		return fmt.Errorf("write metalman-edge token: %w", err)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("replace metalman-edge token: %w", err),
				fmt.Errorf("remove temporary metalman-edge token: %w", removeErr),
			)
		}

		return fmt.Errorf("replace metalman-edge token: %w", err)
	}

	return nil
}
