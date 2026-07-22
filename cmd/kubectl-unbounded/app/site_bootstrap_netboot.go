// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"time"

	"github.com/spf13/cobra"

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
