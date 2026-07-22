// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

const defaultBootstrapNetbootTimeout = 30 * time.Minute

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
}

type bootstrapNetbootState struct {
	originalEndpointRef string
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
