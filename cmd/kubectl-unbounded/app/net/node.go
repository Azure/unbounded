// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// newNodeRootCommand builds node operation commands.
func newNodeRootCommand(rt *pluginRuntime) *cobra.Command {
	fetch := defaultNodeStatusFetchOptions()
	cmd := &cobra.Command{
		Use:     "node",
		Aliases: []string{"nodes"},
		Short:   "Node status operations",
	}
	addNodeStatusFetchFlags(cmd.PersistentFlags(), fetch)
	cmd.AddCommand(
		newNodeListCommand(rt, fetch),
		newNodeLogsCommand(rt),
		newNodeExecCommand(rt),
		newNodeShowCommand(rt, fetch),
	)

	return cmd
}

// newNodeListCommand builds node list command backed by /status/json.
func newNodeListCommand(rt *pluginRuntime, fetch nodeStatusFetchOptions) *cobra.Command {
	var (
		output           string
		color            string
		watch            bool
		suppressWarnings bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List nodes using controller /status/json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch {
				fetchOpts := nodeStatusFetchFromCommand(cmd).merged(fetch)

				return runWatch(cmd.Context(), rt, cmd, fetchOpts, color, func(w io.Writer, status clusterStatusResponse, useColor bool) error {
					rows := buildNodeRows(status)
					wide := output == "wide"

					return printNodeRowsTable(w, rows, useColor, wide)
				}, &watchOpts{
					renderSummary: func(w io.Writer, summary *clusterSummary, useColor bool) error {
						rows := buildNodeRowsFromSummary(*summary)
						wide := output == "wide"

						return printNodeRowsTable(w, rows, useColor, wide)
					},
				})
			}

			return runNodeList(rt, cmd, fetch, output, color, suppressWarnings)
		},
	}

	addNodeListFlags(cmd.Flags(), &output, &color)
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Watch live updates via WebSocket")
	cmd.Flags().BoolVar(&suppressWarnings, "suppress-warnings", false, "Hide warnings from output")
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"table", "wide", "json"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("color", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"auto", "always", "never"}, cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

// nodeStatusFetchOptions configures how node commands query controller status.
type nodeStatusFetchOptions struct {
	controllerService  string
	controllerSelector string
	controllerDeploy   string
	controllerPort     string
	timeout            time.Duration
}

// defaultNodeStatusFetchOptions returns default fetch settings.
func defaultNodeStatusFetchOptions() nodeStatusFetchOptions {
	return nodeStatusFetchOptions{
		controllerService:  defaultControllerService,
		controllerSelector: defaultControllerSelector,
		controllerDeploy:   defaultControllerDeploy,
		controllerPort:     defaultControllerPort,
		timeout:            15 * time.Second,
	}
}

// addNodeStatusFetchFlags adds controller status fetch related flags.
func addNodeStatusFetchFlags(flagSet *pflag.FlagSet, opts nodeStatusFetchOptions) {
	flagSet.String("controller-service", opts.controllerService, "Controller service name")
	flagSet.String("controller-selector", opts.controllerSelector, "Controller pod selector for status fallback")
	flagSet.String("controller-deployment", opts.controllerDeploy, "Controller deployment name for status fallback")
	flagSet.String("controller-port", opts.controllerPort, "Controller service port")
	flagSet.Duration("timeout", opts.timeout, "Request timeout")
}

// nodeStatusFetchFromCommand resolves fetch options from command flags.
func nodeStatusFetchFromCommand(cmd *cobra.Command) nodeStatusFetchOptions {
	service, _ := cmd.Flags().GetString("controller-service")   //nolint:errcheck
	selector, _ := cmd.Flags().GetString("controller-selector") //nolint:errcheck
	deploy, _ := cmd.Flags().GetString("controller-deployment") //nolint:errcheck
	port, _ := cmd.Flags().GetString("controller-port")         //nolint:errcheck
	timeout, _ := cmd.Flags().GetDuration("timeout")            //nolint:errcheck

	return nodeStatusFetchOptions{
		controllerService:  service,
		controllerSelector: selector,
		controllerDeploy:   deploy,
		controllerPort:     port,
		timeout:            timeout,
	}
}

// addNodeListFlags adds list output/color flags.
func addNodeListFlags(flagSet *pflag.FlagSet, output, color *string) {
	flagSet.StringVarP(output, "output", "o", "table", "Output format: table|wide|json")
	flagSet.StringVarP(color, "color", "C", "auto", "Colorize output: auto|always|never (pass -C with no value for always)")

	if flag := flagSet.Lookup("color"); flag != nil {
		flag.NoOptDefVal = "always"
	}
}

// runNodeList executes node list behavior.
func runNodeList(rt *pluginRuntime, cmd *cobra.Command, baseFetch nodeStatusFetchOptions, output, color string, suppressWarnings bool) error {
	fetchOpts := baseFetch

	override := nodeStatusFetchFromCommand(cmd)
	if override.controllerService != "" {
		fetchOpts.controllerService = override.controllerService
	}

	if override.controllerSelector != "" {
		fetchOpts.controllerSelector = override.controllerSelector
	}

	if override.controllerDeploy != "" {
		fetchOpts.controllerDeploy = override.controllerDeploy
	}

	if override.controllerPort != "" {
		fetchOpts.controllerPort = override.controllerPort
	}

	if override.timeout > 0 {
		fetchOpts.timeout = override.timeout
	}

	status, err := fetchClusterStatus(rt, cmd, fetchOpts)
	if err != nil {
		return err
	}

	rows := buildNodeRows(status)
	useColor := shouldUseColor(cmd.OutOrStdout(), color)

	if !suppressWarnings {
		printWarnings(cmd.OutOrStdout(), collectWarnings(status), useColor)
	}

	switch output {
	case "json":
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}

		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)

		return err
	case "table":
		return printNodeRowsTable(cmd.OutOrStdout(), rows, useColor, false)
	case "wide":
		return printNodeRowsTable(cmd.OutOrStdout(), rows, useColor, true)
	default:
		return fmt.Errorf("unsupported output: %s", output)
	}
}

// fetchClusterStatus gets cluster status using service proxy and pod-forward fallback.
func fetchClusterStatus(rt *pluginRuntime, cmd *cobra.Command, opts nodeStatusFetchOptions) (clusterStatusResponse, error) {
	var status clusterStatusResponse

	ns, err := rt.namespace()
	if err != nil {
		return status, err
	}

	client, err := rt.kubeClient()
	if err != nil {
		return status, err
	}

	cfg, err := rt.restConfig()
	if err != nil {
		return status, err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	raw, err := fetchStatusViaServiceProxy(ctx, client, ns, opts.controllerService, opts.controllerPort)
	if err != nil {
		raw, err = fetchStatusViaPortForward(ctx, client, cfg, ns, opts.controllerDeploy, opts.controllerSelector, opts.controllerPort, opts.timeout)
		if err != nil {
			return status, fmt.Errorf("fetch /status/json failed via service proxy (%s) and pod port-forward (%s)", opts.controllerService, err)
		}
	}

	if err := json.Unmarshal(raw, &status); err != nil {
		return status, fmt.Errorf("decode /status/json: %w", err)
	}

	return status, nil
}

// newNodeLogsCommand shows CNI node-agent logs for a specific Kubernetes node.
func newNodeLogsCommand(rt *pluginRuntime) *cobra.Command {
	var (
		selector   string
		container  string
		follow     bool
		previous   bool
		tail       int64
		since      time.Duration
		sinceTime  string
		timestamps bool
	)

	cmd := &cobra.Command{
		Use:   "logs NODE_NAME",
		Short: "Show CNI node-agent logs for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]

			ns, err := rt.namespace()
			if err != nil {
				return err
			}

			client, err := rt.kubeClient()
			if err != nil {
				return err
			}

			pod, err := resolveNodeAgentPod(cmd.Context(), client, ns, nodeName, selector)
			if err != nil {
				return err
			}

			logOpts := &corev1.PodLogOptions{
				Container:  container,
				Follow:     follow,
				Previous:   previous,
				Timestamps: timestamps,
			}
			if cmd.Flags().Changed("tail") {
				logOpts.TailLines = &tail
			}

			if cmd.Flags().Changed("since") {
				logOpts.SinceSeconds = ptrInt64(int64(since.Seconds()))
			}

			if cmd.Flags().Changed("since-time") {
				parsed, parseErr := time.Parse(time.RFC3339, sinceTime)
				if parseErr != nil {
					return fmt.Errorf("invalid --since-time %q: %w", sinceTime, parseErr)
				}

				logOpts.SinceTime = &v1.Time{Time: parsed}
			}

			req := client.CoreV1().Pods(ns).GetLogs(pod.Name, logOpts)

			stream, err := req.Stream(cmd.Context())
			if err != nil {
				return err
			}

			defer func() { _ = stream.Close() }() //nolint:errcheck

			_, err = io.Copy(cmd.OutOrStdout(), stream)

			return err
		},
	}
	cmd.ValidArgsFunction = nodeNameCompletion(rt, true)
	cmd.Flags().StringVarP(&selector, "selector", "l", defaultNodeSelector, "Label selector for node-agent pods")
	cmd.Flags().StringVar(&container, "container", defaultNodeContainer, "Container name in node-agent pod")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream logs")
	cmd.Flags().BoolVar(&previous, "previous", false, "Use previous container instance logs")
	cmd.Flags().Int64Var(&tail, "tail", -1, "Lines of recent logs to show")
	cmd.Flags().DurationVar(&since, "since", 0, "Only return logs newer than a relative duration like 5s, 2m, or 3h")
	cmd.Flags().StringVar(&sinceTime, "since-time", "", "Only return logs after a specific RFC3339 timestamp")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "Include timestamps on each line")

	return cmd
}

// newNodeExecCommand executes a command in the CNI node-agent pod selected by node name.
func newNodeExecCommand(rt *pluginRuntime) *cobra.Command {
	var (
		selector     string
		container    string
		stdinEnabled bool
		tty          bool
	)

	cmd := &cobra.Command{
		Use:   "exec NODE_NAME [-c CONTAINER] [-i] [-t] [-- COMMAND [ARG...]]",
		Short: "Execute a command in the CNI node-agent pod for a node",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]

			dash := cmd.ArgsLenAtDash()
			if len(args) > 1 && dash < 0 {
				return fmt.Errorf("command arguments must follow --, for example: kubectl unbounded node exec %s -ti -- /bin/sh", nodeName)
			}

			execCommand := []string{"/bin/sh"}

			if dash >= 0 {
				if dash != 1 {
					return fmt.Errorf("unexpected arguments before --; expected only NODE_NAME before --")
				}

				if dash >= len(args) {
					return errors.New("missing command after --")
				}

				execCommand = args[dash:]
			}

			ns, err := rt.namespace()
			if err != nil {
				return err
			}

			client, err := rt.kubeClient()
			if err != nil {
				return err
			}

			cfg, err := rt.restConfig()
			if err != nil {
				return err
			}

			pod, err := resolveNodeAgentPod(cmd.Context(), client, ns, nodeName, selector)
			if err != nil {
				return err
			}

			return execInPod(
				cmd.Context(),
				client,
				cfg,
				ns,
				pod.Name,
				container,
				execCommand,
				tty,
				func() io.Reader {
					if stdinEnabled {
						return os.Stdin
					}

					return nil
				}(),
				os.Stdout,
				os.Stderr,
			)
		},
	}
	cmd.ValidArgsFunction = nodeNameCompletion(rt, false)
	cmd.Flags().StringVarP(&selector, "selector", "l", defaultNodeSelector, "Label selector for node-agent pods")
	cmd.Flags().StringVar(&container, "container", defaultNodeContainer, "Container name in node-agent pod")
	cmd.Flags().BoolVarP(&stdinEnabled, "stdin", "i", false, "Pass stdin to the container")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "Stdin is a TTY")

	return cmd
}

// nodeNameCompletion completes NODE_NAME positional args using Kubernetes Nodes.
func nodeNameCompletion(rt *pluginRuntime, singleArg bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			if singleArg {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}

			return nil, cobra.ShellCompDirectiveDefault
		}

		names, err := listNodeNamesForCompletion(rt, cmd.Context(), toComplete)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		sort.Strings(names)

		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// nodeShowCompletion completes node show positional arguments.
func nodeShowCompletion(rt *pluginRuntime, baseFetch nodeStatusFetchOptions) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			names, err := listNodeNamesForCompletion(rt, cmd.Context(), toComplete)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}

			sort.Strings(names)

			return names, cobra.ShellCompDirectiveNoFileComp
		case 1:
			modes := []string{"peer", "peers", "route", "routes", "bpf", "json"}

			out := make([]string, 0, len(modes))
			for _, mode := range modes {
				if strings.HasPrefix(mode, toComplete) {
					out = append(out, mode)
				}
			}

			return out, cobra.ShellCompDirectiveNoFileComp
		case 2:
			mode := strings.ToLower(strings.TrimSpace(args[1]))
			if mode == "peer" || mode == "peers" {
				names, err := listNodePeeringsForCompletion(rt, cmd, baseFetch, args[0], toComplete)
				if err != nil {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}

				sort.Strings(names)

				return names, cobra.ShellCompDirectiveNoFileComp
			}

			return nil, cobra.ShellCompDirectiveNoFileComp
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
}

// listNodeNamesForCompletion returns node names filtered by prefix for shell completion.
func listNodeNamesForCompletion(rt *pluginRuntime, ctx context.Context, prefix string) ([]string, error) {
	client, err := rt.kubeClient()
	if err != nil {
		return nil, err
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, v1.ListOptions{})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if strings.HasPrefix(node.Name, prefix) {
			names = append(names, node.Name)
		}
	}

	return names, nil
}

// listNodePeeringsForCompletion returns existing peering destination node names for a source node.
func listNodePeeringsForCompletion(rt *pluginRuntime, cmd *cobra.Command, baseFetch nodeStatusFetchOptions, sourceNode, prefix string) ([]string, error) {
	status, err := fetchClusterStatus(rt, cmd, nodeStatusFetchFromCommand(cmd).merged(baseFetch))
	if err != nil {
		return nil, err
	}

	node, ok := nodeStatusByName(status, sourceNode)
	if !ok {
		return nil, fmt.Errorf("node %q not found in status", sourceNode)
	}

	seen := make(map[string]struct{})

	names := make([]string, 0, len(node.Peers))
	for _, peer := range node.Peers {
		name := strings.TrimSpace(peer.Name)
		if name == "" || !strings.HasPrefix(name, prefix) {
			continue
		}

		if _, exists := seen[name]; exists {
			continue
		}

		seen[name] = struct{}{}
		names = append(names, name)
	}

	return names, nil
}

// newNodeShowCommand prints node info and detail tables from cluster status.
func newNodeShowCommand(rt *pluginRuntime, baseFetch nodeStatusFetchOptions) *cobra.Command {
	var (
		color string
		watch bool
	)

	cmd := &cobra.Command{
		Use:   "show NODE_NAME [peer|peers|route|routes|bpf|json] [PEER_NODE]",
		Short: "Show node info, peers, routes, BPF entries, or raw JSON from status data",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch {
				fetchOpts := nodeStatusFetchFromCommand(cmd).merged(baseFetch)
				nodeName := args[0]
				mode := ""
				peerName := ""

				if len(args) >= 2 {
					mode = strings.ToLower(args[1])
				}

				if len(args) == 3 {
					peerName = args[2]
				}

				return runWatch(cmd.Context(), rt, cmd, fetchOpts, color, func(w io.Writer, status clusterStatusResponse, useColor bool) error {
					node, ok := nodeStatusByName(status, nodeName)
					if !ok {
						return fmt.Errorf("node %q not found in controller status", nodeName)
					}

					switch mode {
					case "peer", "peers":
						return printNodePeerings(w, status, node, peerName, useColor)
					case "route", "routes":
						return printNodeRoutes(w, node, useColor)
					case "bpf":
						return printNodeBpf(w, node)
					case "json":
						data, jsonErr := json.MarshalIndent(node, "", "  ")
						if jsonErr != nil {
							return jsonErr
						}

						_, _ = fmt.Fprintf(w, "%s\n", data) //nolint:errcheck

						return nil
					default:
						return printNodeInfoPane(w, status, node, useColor)
					}
				}, nil)
			}

			useColor := shouldUseColor(cmd.OutOrStdout(), color)

			status, err := fetchClusterStatus(rt, cmd, nodeStatusFetchFromCommand(cmd).merged(baseFetch))
			if err != nil {
				return err
			}

			nodeName := args[0]

			node, ok := nodeStatusByName(status, nodeName)
			if !ok {
				return fmt.Errorf("node %q not found in controller status", nodeName)
			}

			if len(args) == 1 {
				return printNodeInfoPane(cmd.OutOrStdout(), status, node, useColor)
			}

			mode := strings.ToLower(args[1])
			switch mode {
			case "peer", "peers":
				var peerName string
				if len(args) == 3 {
					peerName = args[2]
				}

				return printNodePeerings(cmd.OutOrStdout(), status, node, peerName, useColor)
			case "route", "routes":
				return printNodeRoutes(cmd.OutOrStdout(), node, useColor)
			case "bpf":
				return printNodeBpf(cmd.OutOrStdout(), node)
			case "json":
				data, jsonErr := json.MarshalIndent(node, "", "  ")
				if jsonErr != nil {
					return jsonErr
				}

				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)

				return err
			default:
				return fmt.Errorf("unsupported show subcommand %q, expected peer(s), route(s), bpf, or json", args[1])
			}
		},
	}
	cmd.ValidArgsFunction = nodeShowCompletion(rt, baseFetch)
	cmd.Flags().StringVarP(&color, "color", "C", "auto", "Colorize output: auto|always|never (pass -C with no value for always)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Watch live updates via WebSocket")

	if flag := cmd.Flags().Lookup("color"); flag != nil {
		flag.NoOptDefVal = "always"
	}

	_ = cmd.RegisterFlagCompletionFunc("color", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{"auto", "always", "never"}, cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

// merged combines command overrides and base defaults.
func (o nodeStatusFetchOptions) merged(base nodeStatusFetchOptions) nodeStatusFetchOptions {
	out := base
	if o.controllerService != "" {
		out.controllerService = o.controllerService
	}

	if o.controllerSelector != "" {
		out.controllerSelector = o.controllerSelector
	}

	if o.controllerDeploy != "" {
		out.controllerDeploy = o.controllerDeploy
	}

	if o.controllerPort != "" {
		out.controllerPort = o.controllerPort
	}

	if o.timeout > 0 {
		out.timeout = o.timeout
	}

	return out
}

// resolveNodeAgentPod finds the node-agent pod for a given node.
func resolveNodeAgentPod(ctx context.Context, client *kubernetes.Clientset, ns, nodeName, selector string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(ns).List(ctx, v1.ListOptions{
		LabelSelector: selector,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no node-agent pod found for node %q in namespace %q", nodeName, ns)
	}

	sort.Slice(pods.Items, func(i, j int) bool {
		a := pods.Items[i]

		b := pods.Items[j]
		if a.Status.Phase == corev1.PodRunning && b.Status.Phase != corev1.PodRunning {
			return true
		}

		if b.Status.Phase == corev1.PodRunning && a.Status.Phase != corev1.PodRunning {
			return false
		}

		return a.Name < b.Name
	})

	return &pods.Items[0], nil
}

// execInPod executes a command inside a pod container.
func execInPod(
	ctx context.Context,
	client *kubernetes.Clientset,
	cfg *rest.Config,
	ns string,
	podName string,
	container string,
	command []string,
	tty bool,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	stdinEnabled := stdin != nil
	stderrEnabled := stderr != nil && !tty
	req := client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(ns).
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container,
		Command:   command,
		Stdin:     stdinEnabled,
		Stdout:    true,
		Stderr:    stderrEnabled,
		TTY:       tty,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, req.URL())
	if err != nil {
		return err
	}

	if tty {
		if inFile, ok := stdin.(*os.File); ok && term.IsTerminal(int(inFile.Fd())) {
			state, rawErr := term.MakeRaw(int(inFile.Fd()))
			if rawErr == nil {
				defer func() { _ = term.Restore(int(inFile.Fd()), state) }() //nolint:errcheck
			}
		}
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: func() io.Writer {
			if stderrEnabled {
				return stderr
			}

			return nil
		}(),
		Tty: tty,
	})
}

// nodeStatusByName returns the node status entry with the given node name.
func nodeStatusByName(status clusterStatusResponse, nodeName string) (statusv1alpha1.NodeStatusResponse, bool) {
	for _, n := range status.Nodes {
		if strings.EqualFold(n.NodeInfo.Name, nodeName) {
			return n, true
		}
	}

	return statusv1alpha1.NodeStatusResponse{}, false
}

// fetchStatusViaServiceProxy fetches status through the aggregated API endpoint.
func fetchStatusViaServiceProxy(ctx context.Context, client *kubernetes.Clientset, ns, service, port string) ([]byte, error) {
	return client.CoreV1().RESTClient().
		Get().
		AbsPath(
			"/apis/status.net.unbounded-kube.io/v1alpha1/status/json",
		).
		DoRaw(ctx)
}

// fetchStatusViaPortForward fetches status by port-forwarding to one controller pod.
func fetchStatusViaPortForward(
	ctx context.Context,
	client *kubernetes.Clientset,
	cfg *rest.Config,
	ns string,
	deployName string,
	selector string,
	remotePort string,
	timeout time.Duration,
) ([]byte, error) {
	pods, err := podsForController(ctx, client, ns, deployName, selector)
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no controller pods found in namespace %q", ns)
	}

	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	podName := pods.Items[0].Name

	reqURL := client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(ns).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdyRoundTripperForConfig(cfg)
	if err != nil {
		return nil, err
	}

	stopCh := make(chan struct{}, 1)
	readyCh := make(chan struct{})
	errCh := make(chan error, 1)

	fw, err := newPortForwarder(reqURL, transport, upgrader, []string{"0:" + remotePort}, stopCh, readyCh, io.Discard, io.Discard, []string{"127.0.0.1"})
	if err != nil {
		return nil, err
	}

	go func() {
		errCh <- fw.ForwardPorts()
	}()

	select {
	case <-readyCh:
	case err := <-errCh:
		return nil, fmt.Errorf("port-forward controller pod %s failed: %w", podName, err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	defer close(stopCh)

	fwdPorts, err := fw.GetPorts()
	if err != nil {
		return nil, err
	}

	if len(fwdPorts) == 0 {
		return nil, errors.New("port-forward did not expose a local port")
	}

	localPort := fwdPorts[0].Local

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/status/json", localPort), nil)
	if err != nil {
		return nil, err
	}

	// Request an HMAC viewer token for authentication. When port-forwarding
	// directly to the controller pod, the API server front-proxy is bypassed
	// so the controller requires an HMAC token.
	if viewerToken, tokenErr := requestViewerToken(cfg); tokenErr == nil {
		req.Header.Set("Authorization", "Bearer "+viewerToken)
	}

	// Fetch the controller CA from the ConfigMap for TLS verification.
	caPool := x509.NewCertPool()

	if cm, cmErr := client.CoreV1().ConfigMaps(ns).Get(ctx, "unbounded-net-serving-ca", v1.GetOptions{}); cmErr == nil {
		if caPEM := []byte(cm.Data["ca.crt"]); len(caPEM) > 0 {
			caPool.AppendCertsFromPEM(caPEM)
		}
	}

	tlsClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    caPool,
				ServerName: fmt.Sprintf("unbounded-net-controller.%s.svc", ns),
			},
		},
	}

	resp, err := tlsClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("controller /status/json returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return body, nil
}

// podsForController returns controller pods from deployment selector, falling back to label selector.
func podsForController(ctx context.Context, client *kubernetes.Clientset, ns, deployName, selector string) (*corev1.PodList, error) {
	if deployName != "" {
		deploy, err := client.AppsV1().Deployments(ns).Get(ctx, deployName, v1.GetOptions{})
		if err == nil {
			deploySelector := v1.FormatLabelSelector(deploy.Spec.Selector)
			return client.CoreV1().Pods(ns).List(ctx, v1.ListOptions{LabelSelector: deploySelector})
		}
	}

	return client.CoreV1().Pods(ns).List(ctx, v1.ListOptions{LabelSelector: selector})
}
