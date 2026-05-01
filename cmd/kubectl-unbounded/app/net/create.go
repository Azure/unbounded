// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/printers"
)

// healthCheckFlags contains optional health check settings shared by multiple create commands.
type healthCheckFlags struct {
	enabledSet          bool
	enabled             bool
	detectMultiplier    int32
	detectMultiplierSet bool
	receiveInterval     string
	receiveSet          bool
	transmitInterval    string
	transmitSet         bool
	tunnelMTU           int32
	tunnelMTUSet        bool
	tunnelProtocol      string
	tunnelProtocolSet   bool
}

// addToFlags registers health check flags with a command.
func (b *healthCheckFlags) addToFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&b.enabled, "health-check-enabled", false, "Enable UDP health probes over tunnels")
	cmd.Flags().Int32Var(&b.detectMultiplier, "health-check-detect-multiplier", 0, "Number of missed probes before marking a peer down")
	cmd.Flags().StringVar(&b.receiveInterval, "health-check-receive-interval", "", "Min interval between received probes before declaring down, e.g. 300ms")
	cmd.Flags().StringVar(&b.transmitInterval, "health-check-transmit-interval", "", "Interval between transmitted health probes, e.g. 300ms")
	cmd.Flags().Int32Var(&b.tunnelMTU, "tunnel-mtu", 0, "MTU for tunnel interfaces in this scope")
	cmd.Flags().StringVar(&b.tunnelProtocol, "tunnel-protocol", "", "Tunnel encapsulation protocol (WireGuard, GENEVE, or Auto)")
	_ = cmd.RegisterFlagCompletionFunc("tunnel-protocol", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"WireGuard", "GENEVE", "Auto"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("health-check-receive-interval", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return cobra.AppendActiveHelp(nil, "Minimum interval between received health probes before declaring down. Duration, e.g. 300ms or 1s"), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("health-check-transmit-interval", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return cobra.AppendActiveHelp(nil, "Interval between transmitted health probes. Duration, e.g. 300ms or 1s"), cobra.ShellCompDirectiveNoFileComp
	})
}

// selectedFrom sets explicitness bits from changed flags.
func (b *healthCheckFlags) selectedFrom(cmd *cobra.Command) {
	b.enabledSet = cmd.Flags().Changed("health-check-enabled")
	b.detectMultiplierSet = cmd.Flags().Changed("health-check-detect-multiplier")
	b.receiveSet = cmd.Flags().Changed("health-check-receive-interval")
	b.transmitSet = cmd.Flags().Changed("health-check-transmit-interval")
	b.tunnelMTUSet = cmd.Flags().Changed("tunnel-mtu")
	b.tunnelProtocolSet = cmd.Flags().Changed("tunnel-protocol")
}

// toObject builds object form for spec.healthCheckSettings or returns nil when unset.
func (b *healthCheckFlags) toObject() map[string]interface{} {
	out := map[string]interface{}{}
	if b.enabledSet {
		out["enabled"] = b.enabled
	}

	if b.detectMultiplierSet {
		out["detectMultiplier"] = b.detectMultiplier
	}

	if b.receiveSet {
		out["receiveInterval"] = b.receiveInterval
	}

	if b.transmitSet {
		out["transmitInterval"] = b.transmitInterval
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// tunnelMTUValue returns the MTU value if set, or 0.
func (b *healthCheckFlags) tunnelMTUValue() int32 {
	if b.tunnelMTUSet {
		return b.tunnelMTU
	}

	return 0
}

// tunnelProtocolValue returns the tunnel protocol if set, or empty string.
func (b *healthCheckFlags) tunnelProtocolValue() string {
	if b.tunnelProtocolSet {
		return b.tunnelProtocol
	}

	return ""
}

// createOptions contains shared create command options.
type createOptions struct {
	runtime  *pluginRuntime
	output   string
	dryRun   string
	fieldMgr string
}

// validate validates shared create options.
func (o *createOptions) validate() error {
	switch o.output {
	case "", "name", "yaml", "json":
	default:
		return fmt.Errorf("unsupported output %q, expected one of: name|yaml|json", o.output)
	}

	switch o.dryRun {
	case "", "none", "client", "server":
	default:
		return fmt.Errorf("unsupported dry-run mode %q, expected one of: none|client|server", o.dryRun)
	}

	return nil
}

// createResource creates one unstructured resource or prints client dry-run output.
func (o *createOptions) createResource(cmd *cobra.Command, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	if err := o.validate(); err != nil {
		return err
	}

	if o.dryRun == "client" {
		return printObject(cmd.OutOrStdout(), obj, o.output, gvr.Resource)
	}

	client, err := o.runtime.dynamicClient()
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}

	opts := v1.CreateOptions{FieldManager: o.fieldMgr}
	if o.dryRun == "server" {
		opts.DryRun = []string{v1.DryRunAll}
	}

	created, err := client.Resource(gvr).Create(cmd.Context(), obj, opts)
	if err != nil {
		return err
	}

	return printObject(cmd.OutOrStdout(), created, o.output, gvr.Resource)
}

// printObject renders output with kubectl-style defaults.
func printObject(w io.Writer, obj *unstructured.Unstructured, output, resource string) error {
	switch output {
	case "", "name":
		_, err := fmt.Fprintf(w, "%s/%s created\n", resource, obj.GetName())
		return err
	case "json":
		p := printers.JSONPrinter{}
		return p.PrintObj(obj, w)
	case "yaml":
		p := printers.YAMLPrinter{}
		return p.PrintObj(obj, w)
	default:
		return fmt.Errorf("unsupported output format %q", output)
	}
}

// addCreateCommonFlags adds shared flags to all create commands.
func addCreateCommonFlags(cmd *cobra.Command, o *createOptions) {
	cmd.Flags().StringVarP(&o.output, "output", "o", "name", "Output format. One of: name|yaml|json")
	cmd.Flags().StringVar(&o.dryRun, "dry-run", "none", "Must be \"none\", \"client\", or \"server\"")
	cmd.Flags().StringVar(&o.fieldMgr, "field-manager", "kubectl-unbounded", "Name associated with managed fields for create requests")
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"name", "yaml", "json"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("dry-run", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"none", "client", "server"}, cobra.ShellCompDirectiveNoFileComp
	})
}

// parseKeyValueSlice parses repeated key=value pairs to a map.
func parseKeyValueSlice(values []string) (map[string]string, error) {
	out := map[string]string{}

	for _, kv := range values {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid key=value pair %q", kv)
		}

		out[key] = value
	}

	return out, nil
}

// requireNameArg returns a ValidArgsFunction that shows a <name> placeholder
// when no positional argument has been provided yet.
func requireNameArg(resource string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return []string{"<name>\tName for the " + resource + " resource"}, cobra.ShellCompDirectiveNoFileComp
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// registerActiveHelp registers an active help hint for a flag's value completion.
func registerActiveHelp(cmd *cobra.Command, flagName, hint string) {
	_ = cmd.RegisterFlagCompletionFunc(flagName, func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return cobra.AppendActiveHelp(nil, hint), cobra.ShellCompDirectiveNoFileComp
	})
}

// newCreateRootCommand builds the create command group.
func newCreateRootCommand(rt *pluginRuntime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create unbounded custom resources",
	}
	cmd.AddCommand(
		newCreateSiteCommand(rt),
		newCreateGatewayPoolCommand(rt),
		newCreateSitePeeringCommand(rt),
		newCreateSiteGatewayPoolAssignmentCommand(rt),
		newCreateGatewayPoolPeeringCommand(rt),
	)

	return cmd
}

// newCreateSiteCommand creates the Site command.
func newCreateSiteCommand(rt *pluginRuntime) *cobra.Command {
	o := &createOptions{runtime: rt, output: "name", dryRun: "none", fieldMgr: "kubectl-unbounded"}

	var (
		nodeCIDRs          []string
		nonMasqueradeCIDRs []string
		localCIDRs         []string
		podCIDRBlocks      []string
		podNodeRegex       []string
		podPriority        int32
		podPrioritySet     bool
		podAssignEnabled   bool
		podAssignSet       bool
		podIPv4BlockSize   int
		podIPv6BlockSize   int
		noManageCNI        bool
		noManageCNISet     bool
	)

	hc := &healthCheckFlags{}

	cmd := &cobra.Command{
		Use:     "site NAME",
		Short:   "Create a Site resource",
		Aliases: []string{"st"},
		Long: `Create a Site resource that defines a network boundary in the unbounded-net mesh.

A site represents a set of nodes sharing common network CIDRs. Nodes are
assigned to sites by the controller based on their IP addresses matching the
site's node CIDRs. Each site can optionally manage pod CIDR assignment and
CNI configuration for its member nodes.

For clusters with an existing CNI plugin (e.g. Cilium, Calico), use
--no-manage-cni-plugin and --pod-assignment-enabled=false. The pod CIDR
blocks are still required to define the address ranges for inter-site
routing, but the controller will not assign per-node pod CIDRs or write
CNI configuration.`,
		Example: `  # Create a basic site
  kubectl unbounded net create site my-site \
    --node-cidr 10.0.0.0/16 \
    --pod-cidr-block 10.244.0.0/16

  # Create a site with non-masquerade CIDRs
  kubectl unbounded net create site my-site \
    --node-cidr 10.0.0.0/16 \
    --pod-cidr-block 10.244.0.0/16 \
    --non-masquerade-cidr 10.244.0.0/16

  # Create a site with an existing CNI plugin (e.g. Cilium, Calico).
  # Pod CIDR assignment is disabled; the pod CIDRs are only used for
  # inter-site routing.
  kubectl unbounded net create site my-site \
    --node-cidr 10.0.0.0/16 \
    --pod-cidr-block 10.244.0.0/16 \
    --pod-assignment-enabled=false \
    --no-manage-cni-plugin

  # Dry-run to preview the resource
  kubectl unbounded net create site my-site \
    --node-cidr 10.0.0.0/16 \
    --pod-cidr-block 10.244.0.0/16 \
    --dry-run=client -o yaml`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: requireNameArg("site"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc.selectedFrom(cmd)
			podPrioritySet = cmd.Flags().Changed("pod-assignment-priority")
			podAssignSet = cmd.Flags().Changed("pod-assignment-enabled")
			noManageCNISet = cmd.Flags().Changed("no-manage-cni-plugin")

			if len(nodeCIDRs) == 0 {
				return errors.New("at least one --node-cidr is required")
			}

			if len(podCIDRBlocks) == 0 {
				return errors.New("at least one --pod-cidr-block is required")
			}

			spec := map[string]interface{}{
				"nodeCidrs": nodeCIDRs,
			}
			if noManageCNISet {
				spec["manageCniPlugin"] = !noManageCNI
			}

			if len(nonMasqueradeCIDRs) > 0 {
				spec["nonMasqueradeCIDRs"] = nonMasqueradeCIDRs
			}

			if len(localCIDRs) > 0 {
				spec["localCidrs"] = localCIDRs
			}

			podAssignment := map[string]interface{}{}
			if podAssignSet {
				podAssignment["assignmentEnabled"] = podAssignEnabled
			}

			if len(podCIDRBlocks) > 0 {
				podAssignment["cidrBlocks"] = podCIDRBlocks
			}

			if podPrioritySet {
				podAssignment["priority"] = podPriority
			}

			if len(podNodeRegex) > 0 {
				podAssignment["nodeRegex"] = podNodeRegex
			}

			nodeBlockSizes := map[string]interface{}{}
			if cmd.Flags().Changed("pod-node-block-size-ipv4") {
				nodeBlockSizes["ipv4"] = podIPv4BlockSize
			}

			if cmd.Flags().Changed("pod-node-block-size-ipv6") {
				nodeBlockSizes["ipv6"] = podIPv6BlockSize
			}

			if len(nodeBlockSizes) > 0 {
				podAssignment["nodeBlockSizes"] = nodeBlockSizes
			}

			if len(podAssignment) > 0 {
				spec["podCidrAssignments"] = []interface{}{podAssignment}
			}

			if hcObj := hc.toObject(); hcObj != nil {
				spec["healthCheckSettings"] = hcObj
			}

			if mtu := hc.tunnelMTUValue(); mtu > 0 {
				spec["tunnelMTU"] = mtu
			}

			if et := hc.tunnelProtocolValue(); et != "" {
				spec["tunnelProtocol"] = et
			}

			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "net.unbounded-cloud.io/v1alpha1",
					"kind":       "Site",
					"metadata": map[string]interface{}{
						"name": args[0],
					},
					"spec": spec,
				},
			}

			return o.createResource(cmd, supportedCreateResources["site"], obj)
		},
	}

	addCreateCommonFlags(cmd, o)
	cmd.Flags().StringSliceVar(&nodeCIDRs, "node-cidr", nil, "Network CIDR that identifies this site's nodes, e.g. 10.0.0.0/16 (repeatable, required)")
	cmd.Flags().StringSliceVar(&nonMasqueradeCIDRs, "non-masquerade-cidr", nil, "CIDR that should not be SNATed when leaving this site (repeatable)")
	cmd.Flags().StringSliceVar(&localCIDRs, "local-cidr", nil, "CIDR considered local to this site, not tunneled (repeatable)")
	cmd.Flags().BoolVar(&noManageCNI, "no-manage-cni-plugin", false, "Disable CNI configuration and same-site tunnel peers")
	cmd.Flags().StringSliceVar(&podCIDRBlocks, "pod-cidr-block", nil, "Pod CIDR pool to allocate per-node pod subnets from, e.g. 10.244.0.0/16 (repeatable, required)")
	cmd.Flags().StringSliceVar(&podNodeRegex, "pod-node-regex", nil, `Node name regex for this assignment rule, e.g. "aks-nodepool1-.*" (repeatable)`)
	cmd.Flags().Int32Var(&podPriority, "pod-assignment-priority", 100, "Priority for podCidrAssignments rule")
	cmd.Flags().BoolVar(&podAssignEnabled, "pod-assignment-enabled", true, "Set assignmentEnabled for podCidrAssignments rule")
	cmd.Flags().IntVar(&podIPv4BlockSize, "pod-node-block-size-ipv4", 0, "Per-node IPv4 mask size for podCidrAssignments")
	cmd.Flags().IntVar(&podIPv6BlockSize, "pod-node-block-size-ipv6", 0, "Per-node IPv6 mask size for podCidrAssignments")
	hc.addToFlags(cmd)
	_ = cmd.MarkFlagRequired("node-cidr")      //nolint:errcheck
	_ = cmd.MarkFlagRequired("pod-cidr-block") //nolint:errcheck
	registerActiveHelp(cmd, "node-cidr", `Network CIDR that uniquely identifies this site's nodes. Format: <network>/<prefix>, e.g. 10.0.0.0/16`)
	registerActiveHelp(cmd, "pod-cidr-block", `Pod CIDR pool to allocate per-node pod subnets from. Format: <network>/<prefix>, e.g. 10.244.0.0/16`)
	registerActiveHelp(cmd, "non-masquerade-cidr", `CIDR that should not be SNATed when leaving this site. Format: <network>/<prefix>`)
	registerActiveHelp(cmd, "local-cidr", `CIDR considered local to this site (not tunneled). Format: <network>/<prefix>`)
	registerActiveHelp(cmd, "pod-node-regex", `Regex to match node names for this pod CIDR assignment rule, e.g. "aks-nodepool1-.*"`)

	return cmd
}

// newCreateGatewayPoolCommand creates the GatewayPool command.
func newCreateGatewayPoolCommand(rt *pluginRuntime) *cobra.Command {
	o := &createOptions{runtime: rt, output: "name", dryRun: "none", fieldMgr: "kubectl-unbounded"}

	var (
		poolType          string
		nodeSelectorPairs []string
		routedCIDRs       []string
	)

	hc := &healthCheckFlags{}

	cmd := &cobra.Command{
		Use:     "gatewaypool NAME",
		Short:   "Create a GatewayPool resource",
		Aliases: []string{"gp"},
		Long: `Create a GatewayPool resource that defines a group of gateway nodes.

Gateway pools select nodes via label selectors and configure them as
tunnel endpoints for inter-site or external connectivity. Pools can be
typed as External (for connections outside the cluster) or Internal
(for inter-site links within the cluster).`,
		Example: `  # Create an external gateway pool selecting labeled nodes
  kubectl unbounded net create gatewaypool my-gw \
    --type External \
    --node-selector role=gateway

  # Create a gateway pool with routed CIDRs
  kubectl unbounded net create gatewaypool my-gw \
    --type Internal \
    --node-selector role=gateway \
    --routed-cidr 10.100.0.0/16

  # Preview as YAML
  kubectl unbounded net create gatewaypool my-gw \
    --node-selector role=gateway --dry-run=client -o yaml`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: requireNameArg("gatewaypool"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc.selectedFrom(cmd)

			if len(nodeSelectorPairs) == 0 {
				return errors.New("at least one --node-selector is required")
			}

			nodeSelector, err := parseKeyValueSlice(nodeSelectorPairs)
			if err != nil {
				return err
			}

			spec := map[string]interface{}{
				"nodeSelector": nodeSelector,
			}

			if cmd.Flags().Changed("type") {
				if poolType != "External" && poolType != "Internal" {
					return fmt.Errorf("invalid --type %q: expected External or Internal", poolType)
				}

				spec["type"] = poolType
			}

			if len(routedCIDRs) > 0 {
				spec["routedCidrs"] = routedCIDRs
			}

			if hcObj := hc.toObject(); hcObj != nil {
				spec["healthCheckSettings"] = hcObj
			}

			if mtu := hc.tunnelMTUValue(); mtu > 0 {
				spec["tunnelMTU"] = mtu
			}

			if et := hc.tunnelProtocolValue(); et != "" {
				spec["tunnelProtocol"] = et
			}

			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "net.unbounded-cloud.io/v1alpha1",
					"kind":       "GatewayPool",
					"metadata": map[string]interface{}{
						"name": args[0],
					},
					"spec": spec,
				},
			}

			return o.createResource(cmd, supportedCreateResources["gatewaypool"], obj)
		},
	}

	addCreateCommonFlags(cmd, o)
	cmd.Flags().StringVar(&poolType, "type", "", `Gateway pool type: "External" or "Internal"`)
	cmd.Flags().StringSliceVar(&nodeSelectorPairs, "node-selector", nil, "Node label selector as key=value, e.g. role=gateway (repeatable, required)")
	cmd.Flags().StringSliceVar(&routedCIDRs, "routed-cidr", nil, "CIDR routed through this gateway pool, e.g. 10.100.0.0/16 (repeatable)")
	_ = cmd.MarkFlagRequired("node-selector") //nolint:errcheck
	hc.addToFlags(cmd)
	_ = cmd.RegisterFlagCompletionFunc("type", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"External", "Internal"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("node-selector", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return cobra.AppendActiveHelp(nil, "Node label selector as key=value, e.g. role=gateway"), cobra.ShellCompDirectiveNoFileComp
	})
	registerActiveHelp(cmd, "routed-cidr", `CIDR routed through this gateway pool. Format: <network>/<prefix>, e.g. 10.100.0.0/16`)

	return cmd
}

// newCreateSitePeeringCommand creates the SitePeering command.
func newCreateSitePeeringCommand(rt *pluginRuntime) *cobra.Command {
	o := &createOptions{runtime: rt, output: "name", dryRun: "none", fieldMgr: "kubectl-unbounded"}

	var (
		sites     []string
		enabled   bool
		meshNodes bool
	)

	hc := &healthCheckFlags{}

	cmd := &cobra.Command{
		Use:     "sitepeering NAME",
		Short:   "Create a SitePeering resource",
		Aliases: []string{"spr"},
		Long: `Create a SitePeering resource that establishes connectivity between sites.

A site peering creates WireGuard tunnels between nodes in the specified
sites, enabling pod-to-pod communication across site boundaries. When
meshNodes is enabled, all nodes in the peered sites form a full mesh.`,
		Example: `  # Peer two sites together
  kubectl unbounded net create sitepeering my-peering \
    --site site-a --site site-b

  # Peer sites with full node mesh and health checks
  kubectl unbounded net create sitepeering my-peering \
    --site site-a --site site-b \
    --mesh-nodes \
    --health-check-interval 5s

  # Disable a peering
  kubectl unbounded net create sitepeering my-peering \
    --site site-a --site site-b --enabled=false`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: requireNameArg("sitepeering"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc.selectedFrom(cmd)

			spec := map[string]interface{}{}
			if cmd.Flags().Changed("enabled") {
				spec["enabled"] = enabled
			}

			if len(sites) > 0 {
				spec["sites"] = sites
			}

			if cmd.Flags().Changed("mesh-nodes") {
				spec["meshNodes"] = meshNodes
			}

			if hcObj := hc.toObject(); hcObj != nil {
				spec["healthCheckSettings"] = hcObj
			}

			if mtu := hc.tunnelMTUValue(); mtu > 0 {
				spec["tunnelMTU"] = mtu
			}

			if et := hc.tunnelProtocolValue(); et != "" {
				spec["tunnelProtocol"] = et
			}

			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "net.unbounded-cloud.io/v1alpha1",
					"kind":       "SitePeering",
					"metadata": map[string]interface{}{
						"name": args[0],
					},
					"spec": spec,
				},
			}

			return o.createResource(cmd, supportedCreateResources["sitepeering"], obj)
		},
	}

	addCreateCommonFlags(cmd, o)
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Set enabled")
	cmd.Flags().StringSliceVar(&sites, "site", nil, "Name of a Site to include in this peering (repeatable)")
	cmd.Flags().BoolVar(&meshNodes, "mesh-nodes", false, "Set meshNodes")
	hc.addToFlags(cmd)

	return cmd
}

// newCreateSiteGatewayPoolAssignmentCommand creates SiteGatewayPoolAssignment.
func newCreateSiteGatewayPoolAssignmentCommand(rt *pluginRuntime) *cobra.Command {
	o := &createOptions{runtime: rt, output: "name", dryRun: "none", fieldMgr: "kubectl-unbounded"}

	var (
		enabled      bool
		sites        []string
		gatewayPools []string
	)

	hc := &healthCheckFlags{}

	cmd := &cobra.Command{
		Use:     "sitegatewaypoolassignment NAME",
		Short:   "Create a SiteGatewayPoolAssignment resource",
		Aliases: []string{"sgpa"},
		Long: `Create a SiteGatewayPoolAssignment resource that connects sites to gateway pools.

This resource assigns one or more sites to one or more gateway pools,
enabling traffic from site nodes to be routed through the gateway pool's
nodes. This is used for external connectivity or for connecting remote
sites that cannot peer directly.`,
		Example: `  # Connect a site to an external gateway pool
  kubectl unbounded net create sitegatewaypoolassignment my-sgpa \
    --site my-site \
    --gateway-pool my-gw

  # Connect multiple sites to a gateway pool
  kubectl unbounded net create sitegatewaypoolassignment multi-sgpa \
    --site site-a --site site-b \
    --gateway-pool shared-gw`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: requireNameArg("sitegatewaypoolassignment"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc.selectedFrom(cmd)

			spec := map[string]interface{}{}
			if cmd.Flags().Changed("enabled") {
				spec["enabled"] = enabled
			}

			if len(sites) > 0 {
				spec["sites"] = sites
			}

			if len(gatewayPools) > 0 {
				spec["gatewayPools"] = gatewayPools
			}

			if hcObj := hc.toObject(); hcObj != nil {
				spec["healthCheckSettings"] = hcObj
			}

			if mtu := hc.tunnelMTUValue(); mtu > 0 {
				spec["tunnelMTU"] = mtu
			}

			if et := hc.tunnelProtocolValue(); et != "" {
				spec["tunnelProtocol"] = et
			}

			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "net.unbounded-cloud.io/v1alpha1",
					"kind":       "SiteGatewayPoolAssignment",
					"metadata": map[string]interface{}{
						"name": args[0],
					},
					"spec": spec,
				},
			}

			return o.createResource(cmd, supportedCreateResources["sitegatewaypoolassignment"], obj)
		},
	}

	addCreateCommonFlags(cmd, o)
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Set enabled")
	cmd.Flags().StringSliceVar(&sites, "site", nil, "Name of a Site to include in this peering (repeatable)")
	cmd.Flags().StringSliceVar(&gatewayPools, "gateway-pool", nil, "Name of a GatewayPool to include (repeatable)")
	hc.addToFlags(cmd)

	return cmd
}

// newCreateGatewayPoolPeeringCommand creates GatewayPoolPeering.
func newCreateGatewayPoolPeeringCommand(rt *pluginRuntime) *cobra.Command {
	o := &createOptions{runtime: rt, output: "name", dryRun: "none", fieldMgr: "kubectl-unbounded"}

	var (
		enabled      bool
		gatewayPools []string
	)

	hc := &healthCheckFlags{}

	cmd := &cobra.Command{
		Use:     "gatewaypoolpeering NAME",
		Short:   "Create a GatewayPoolPeering resource",
		Aliases: []string{"gpp"},
		Long: `Create a GatewayPoolPeering resource that connects two gateway pools.

Gateway pool peerings establish tunnels between the nodes of two gateway
pools, enabling traffic to flow between their respective sites. This is
useful for connecting geographically separated clusters or external
network segments.`,
		Example: `  # Peer two gateway pools
  kubectl unbounded net create gatewaypoolpeering my-gpp \
    --gateway-pool gw-east --gateway-pool gw-west

  # Create a disabled peering for later activation
  kubectl unbounded net create gatewaypoolpeering my-gpp \
    --gateway-pool gw-a --gateway-pool gw-b \
    --enabled=false`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: requireNameArg("gatewaypoolpeering"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc.selectedFrom(cmd)

			spec := map[string]interface{}{}
			if cmd.Flags().Changed("enabled") {
				spec["enabled"] = enabled
			}

			if len(gatewayPools) > 0 {
				spec["gatewayPools"] = gatewayPools
			}

			if hcObj := hc.toObject(); hcObj != nil {
				spec["healthCheckSettings"] = hcObj
			}

			if mtu := hc.tunnelMTUValue(); mtu > 0 {
				spec["tunnelMTU"] = mtu
			}

			if et := hc.tunnelProtocolValue(); et != "" {
				spec["tunnelProtocol"] = et
			}

			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "net.unbounded-cloud.io/v1alpha1",
					"kind":       "GatewayPoolPeering",
					"metadata": map[string]interface{}{
						"name": args[0],
					},
					"spec": spec,
				},
			}

			return o.createResource(cmd, supportedCreateResources["gatewaypoolpeering"], obj)
		},
	}

	addCreateCommonFlags(cmd, o)
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Set enabled")
	cmd.Flags().StringSliceVar(&gatewayPools, "gateway-pool", nil, "Name of a GatewayPool to include (repeatable)")
	hc.addToFlags(cmd)

	return cmd
}
