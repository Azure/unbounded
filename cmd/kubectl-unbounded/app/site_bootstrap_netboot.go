// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	defaultBootstrapNetbootTimeout = 30 * time.Minute
	defaultBootstrapPollInterval   = time.Second
)

type siteBootstrapNetbootHandler struct {
	site           string
	machine        string
	interfaceName  string
	address        string
	endpointName   string
	httpPort       int
	kubeconfigPath string
	namespace      string
	metalmanBinary string
	timeout        time.Duration
	routedCIDRs    []string
	resources      client.Client
	kubeClient     kubernetes.Interface
	pollInterval   time.Duration
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
	cmd.Flags().DurationVar(&handler.timeout, "timeout", defaultBootstrapNetbootTimeout, "Maximum time to wait for the designated Node to become Ready")
	cmd.Flags().StringSliceVar(&handler.routedCIDRs, "routed-cidr", nil, "CIDR routed through an ephemeral external gateway (repeatable)")

	_ = cmd.MarkFlagRequired("machine")
	_ = cmd.MarkFlagRequired("interface")
	_ = cmd.MarkFlagRequired("address")

	return cmd
}

func (h *siteBootstrapNetbootHandler) execute(_ context.Context) error {
	return nil
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
		return bootstrapNetbootState{}, fmt.Errorf("Machine %s does not belong to Site %s", h.machine, h.site)
	}

	netboot := machine.Spec.Netboot()
	if netboot == nil {
		return bootstrapNetbootState{}, fmt.Errorf("Machine %s has no netboot configuration", h.machine)
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
		_ = h.resources.Delete(ctx, endpoint)

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
		return "", fmt.Errorf("Deployment %s/%s has no Ready Pods", namespace, deploymentName)
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
