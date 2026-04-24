// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	apiregistrationclientset "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/authn"
	"github.com/Azure/unbounded/internal/net/certmanager"
	"github.com/Azure/unbounded/internal/net/config"
	"github.com/Azure/unbounded/internal/net/controller"
	"github.com/Azure/unbounded/internal/net/metrics"
	"github.com/Azure/unbounded/internal/net/webhook"
	"github.com/Azure/unbounded/internal/version"
)

var gatewayPoolGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

var sitePeeringGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "sitepeerings",
}

var siteGatewayPoolAssignmentGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "sitegatewaypoolassignments",
}

var gatewayPoolPeeringGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "gatewaypoolpeerings",
}

func main() {
	// Initialize klog flags
	klog.InitFlags(nil)

	// Add klog flags to pflag
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	cfg := &config.Config{
		LeaderElection:                config.DefaultLeaderElectionConfig(),
		InformerResyncPeriod:          300 * time.Second,
		AzureTenantID:                 os.Getenv("AZURE_TENANT_ID"),
		RegisterAggregatedAPIServer:   true,
		RequireDashboardAuth:          true,
		StatusWSKeepaliveInterval:     10 * time.Second,
		StatusWSKeepaliveFailureCount: 2,
		NodeTokenLifetime:             4 * time.Hour,
		ViewerTokenLifetime:           30 * time.Minute,
	}
	configFile := "/etc/unbounded-net/config.yaml"
	forceNotLeader := false

	rootCmd := &cobra.Command{
		Use:   "unbounded-net-controller",
		Short: "Kubernetes controller for site-aware networking",
		Long: `unbounded-net-controller manages site-aware networking resources.

It watches for nodes and Site resources to label nodes, assign pod CIDRs based
on site configuration, and maintain SiteNodeSlice and GatewayPool status.`,
		Version: version.Version + " (commit: " + version.GitCommit + ")",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			cfg.ConfigFile = configFile
			if err := applyControllerRuntimeConfig(cmd, cfg, configFile); err != nil {
				return err
			}

			if forceNotLeader && !cfg.LeaderElection.Enabled {
				return fmt.Errorf("--force-not-leader requires --leader-elect=true")
			}

			return cfg.Validate()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cfg, forceNotLeader)
		},
	}

	// Add flags
	flags := rootCmd.Flags()

	// Change version flag from -v to -V to avoid conflict with klog's -v flag
	rootCmd.Flags().BoolP("version", "V", false, "Print version information")
	rootCmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	// General flags
	flags.StringVar(&configFile, "config-file", "/etc/unbounded-net/config.yaml", "Path to runtime YAML config file")
	flags.BoolVar(&forceNotLeader, "force-not-leader", false, "Force this pod to stay standby and never become leader (requires --leader-elect=true)")
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not specified)")
	flags.StringVar(&cfg.ApiserverURL, "apiserver-url", "", "Override Kubernetes API server URL (empty = use default from kubeconfig or in-cluster config)")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "Run single evaluation loop and print proposed changes without applying (not supported for site-based pod CIDR assignments)")
	flags.IntVar(&cfg.HealthPort, "health-port", 9999, "Port for health check HTTP server (0 to disable)")
	flags.IntVar(&cfg.NodeAgentHealthPort, "node-agent-health-port", 9998, "Port where node agents serve their health/status endpoints")
	flags.DurationVar(&cfg.StatusStaleThreshold, "status-stale-threshold", 90*time.Second, "Duration after which a node's pushed status is considered stale")
	flags.DurationVar(&cfg.StatusWSKeepaliveInterval, "status-ws-keepalive-interval", 10*time.Second, "Interval between websocket keepalive pings on controller node status streams (0 to disable)")
	flags.IntVar(&cfg.StatusWSKeepaliveFailureCount, "status-ws-keepalive-failure-count", 2, "Sequential websocket keepalive ping failures before closing node status websocket")
	flags.BoolVar(&cfg.RegisterAggregatedAPIServer, "register-aggregated-apiserver", true, "Serve node status push endpoints via aggregated API server paths")
	flags.BoolVar(&cfg.RequireDashboardAuth, "require-dashboard-auth", true, "Require authentication and RBAC authorization for dashboard and status endpoints")
	flags.DurationVar(&cfg.InformerResyncPeriod, "informer-resync-period", 300*time.Second, "Resync period for Kubernetes informers")
	flags.DurationVar(&cfg.KubeProxyHealthInterval, "kube-proxy-health-interval", 30*time.Second, "Interval between kube-proxy health checks on the controller node (0 to disable)")
	flags.DurationVar(&cfg.NodeTokenLifetime, "node-token-lifetime", 4*time.Hour, "Lifetime of HMAC tokens issued to node agents")
	flags.DurationVar(&cfg.ViewerTokenLifetime, "viewer-token-lifetime", 30*time.Minute, "Lifetime of HMAC tokens issued to dashboard viewers")

	// Leader election flags
	flags.BoolVar(&cfg.LeaderElection.Enabled, "leader-elect", true, "Enable leader election for controller manager")
	flags.DurationVar(&cfg.LeaderElection.LeaseDuration, "leader-elect-lease-duration", 15*time.Second, "Duration that non-leader candidates will wait to force acquire leadership")
	flags.DurationVar(&cfg.LeaderElection.RenewDeadline, "leader-elect-renew-deadline", 5*time.Second, "Duration that the acting leader will retry refreshing leadership before giving up")
	flags.DurationVar(&cfg.LeaderElection.RetryPeriod, "leader-elect-retry-period", 10*time.Second, "Duration the LeaderElector clients should wait between tries of actions")
	flags.StringVar(&cfg.LeaderElection.ResourceNamespace, "leader-elect-resource-namespace", "kube-system", "Namespace for leader election lease")
	flags.StringVar(&cfg.LeaderElection.ResourceName, "leader-elect-resource-name", "unbounded-net-controller", "Name of leader election lease")

	// Group the flags for better help output
	rootCmd.SetUsageTemplate(usageTemplate)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func applyControllerRuntimeConfig(cmd *cobra.Command, cfg *config.Config, configFile string) error {
	runtimeCfg, err := config.LoadRuntimeConfig(configFile)
	if err != nil {
		return err
	}

	flags := cmd.Flags()

	if !flags.Changed("informer-resync-period") {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.InformerResyncPeriod, "controller.informerResyncPeriod"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.InformerResyncPeriod = d
		}
	}

	if !flags.Changed("health-port") && runtimeCfg.Controller.HealthPort != nil {
		cfg.HealthPort = *runtimeCfg.Controller.HealthPort
	}

	if !flags.Changed("node-agent-health-port") && runtimeCfg.Controller.NodeAgentHealthPort != nil {
		cfg.NodeAgentHealthPort = *runtimeCfg.Controller.NodeAgentHealthPort
	}

	if !flags.Changed("status-stale-threshold") {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.StatusStaleThreshold, "controller.statusStaleThreshold"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.StatusStaleThreshold = d
		}
	}

	if !flags.Changed("status-ws-keepalive-interval") && runtimeCfg.Controller.StatusWSKeepaliveInterval != "" {
		d, parseErr := config.ParseDurationField(runtimeCfg.Controller.StatusWSKeepaliveInterval, "controller.statusWebsocketKeepaliveInterval")
		if parseErr != nil {
			return parseErr
		}

		cfg.StatusWSKeepaliveInterval = d
	}

	if !flags.Changed("status-ws-keepalive-failure-count") && runtimeCfg.Controller.StatusWSKeepaliveFailCount != nil {
		cfg.StatusWSKeepaliveFailureCount = *runtimeCfg.Controller.StatusWSKeepaliveFailCount
	}

	if !flags.Changed("register-aggregated-apiserver") && runtimeCfg.Controller.RegisterAggregatedAPIServer != nil {
		cfg.RegisterAggregatedAPIServer = *runtimeCfg.Controller.RegisterAggregatedAPIServer
	}

	if !flags.Changed("require-dashboard-auth") && runtimeCfg.Controller.RequireDashboardAuth != nil {
		cfg.RequireDashboardAuth = *runtimeCfg.Controller.RequireDashboardAuth
	}

	if !flags.Changed("kube-proxy-health-interval") && runtimeCfg.Controller.KubeProxyHealthInterval != "" {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.KubeProxyHealthInterval, "controller.kubeProxyHealthInterval"); parseErr != nil {
			return parseErr
		} else {
			cfg.KubeProxyHealthInterval = d
		}
	}

	if !flags.Changed("leader-elect") && runtimeCfg.Controller.LeaderElection.Enabled != nil {
		cfg.LeaderElection.Enabled = *runtimeCfg.Controller.LeaderElection.Enabled
	}

	if !flags.Changed("leader-elect-lease-duration") {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.LeaderElection.LeaseDuration, "controller.leaderElection.leaseDuration"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.LeaderElection.LeaseDuration = d
		}
	}

	if !flags.Changed("leader-elect-renew-deadline") {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.LeaderElection.RenewDeadline, "controller.leaderElection.renewDeadline"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.LeaderElection.RenewDeadline = d
		}
	}

	if !flags.Changed("leader-elect-retry-period") {
		if d, parseErr := config.ParseDurationField(runtimeCfg.Controller.LeaderElection.RetryPeriod, "controller.leaderElection.retryPeriod"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.LeaderElection.RetryPeriod = d
		}
	}

	if !flags.Changed("leader-elect-resource-namespace") && runtimeCfg.Controller.LeaderElection.ResourceNamespace != "" {
		cfg.LeaderElection.ResourceNamespace = runtimeCfg.Controller.LeaderElection.ResourceNamespace
	}

	if !flags.Changed("leader-elect-resource-name") && runtimeCfg.Controller.LeaderElection.ResourceName != "" {
		cfg.LeaderElection.ResourceName = runtimeCfg.Controller.LeaderElection.ResourceName
	}

	if runtimeCfg.Common.AzureTenantID != "" {
		cfg.AzureTenantID = runtimeCfg.Common.AzureTenantID
	}

	if !flags.Changed("apiserver-url") && runtimeCfg.Common.ApiserverURL != "" {
		cfg.ApiserverURL = runtimeCfg.Common.ApiserverURL
	}

	// Read node.mtu from the shared configmap so the controller can validate
	// that the configured MTU does not exceed any node's detected maximum.
	if runtimeCfg.Node.MTU != nil {
		cfg.NodeMTU = *runtimeCfg.Node.MTU
	}
	// Normalize: treat 0 the same as 1280 (matches node-agent behavior).
	if cfg.NodeMTU == 0 {
		cfg.NodeMTU = 1280
	}

	return nil
}

const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

General Flags:
	--config-file string                      Path to runtime YAML config file (default "/etc/unbounded-net/config.yaml")
	--apiserver-url string                    Override Kubernetes API server URL (empty = use default from kubeconfig or in-cluster config)
    --dry-run                                  Run single evaluation loop and print proposed changes without applying (not supported for site-based pod CIDR assignments)
	--force-not-leader                        Force this pod to stay standby and never become leader (requires --leader-elect=true)
	--health-port int                          Port for health check HTTP server (0 to disable) (default 9999)
	--informer-resync-period duration          Resync period for Kubernetes informers (default 5m0s)
      --kubeconfig string                        Path to kubeconfig file (uses in-cluster config if not specified)
	--node-agent-health-port int               Port where node agents serve their health/status endpoints (default 9998)
      --status-stale-threshold duration          Duration after which a node's pushed status is considered stale (default 90s)
	--status-ws-keepalive-interval duration    Interval between websocket keepalive pings on controller node status streams (0 to disable) (default 10s)
	--status-ws-keepalive-failure-count int    Sequential websocket keepalive ping failures before closing node status websocket (default 2)

	--register-aggregated-apiserver            Serve node status push endpoints via aggregated API server paths (default true)
	--require-dashboard-auth                  Require authentication and RBAC authorization for dashboard and status endpoints (default true)

Leader Election Flags:
      --leader-elect                             Enable leader election for controller manager (default true)
      --leader-elect-lease-duration duration     Duration that non-leader candidates will wait to force acquire leadership (default 15s)
      --leader-elect-renew-deadline duration     Duration that the acting leader will retry refreshing leadership before giving up (default 10s)
      --leader-elect-resource-name string        Name of leader election lease (default "unbounded-net-controller")
      --leader-elect-resource-namespace string   Namespace for leader election lease (default "kube-system")
      --leader-elect-retry-period duration       Duration the LeaderElector clients should wait between tries of actions (default 2s)

Utility Flags:
  -h, --help                                     help for {{.Name}}
  -V, --version                                  Print version information
  -v, --v Level                                  number for the log level verbosity
      --add_dir_header                           If true, adds the file directory to the header of the log messages
      --alsologtostderr                          log to standard error as well as files (no effect when -logtostderr=true)
      --log_backtrace_at traceLocation           when logging hits line file:N, emit a stack trace (default :0)
      --log_dir string                           If non-empty, write log files in this directory (no effect when -logtostderr=true)
      --log_file string                          If non-empty, use this log file (no effect when -logtostderr=true)
      --log_file_max_size uint                   Defines the maximum size a log file can grow to (default 1800)
      --logtostderr                              log to standard error instead of files (default true)
      --one_output                               If true, only write logs to their native severity level
      --skip_headers                             If true, avoid header prefixes in the log messages
      --skip_log_headers                         If true, avoid headers when opening log files
      --stderrthreshold severity                 logs at or above this threshold go to stderr (default 2)
      --vmodule moduleSpec                       comma-separated list of pattern=N settings for file-filtered logging
{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

func run(cfg *config.Config, forceNotLeader bool) error {
	klog.Infof("unbounded-net-controller version=%s commit=%s built=%s", version.Version, version.GitCommit, version.BuildTime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		klog.Info("Received shutdown signal")
		cancel()
	}()

	// Watch the config file for dynamic log level changes.
	go config.WatchConfigLogLevel(ctx, cfg.ConfigFile)

	// Build Kubernetes client
	var (
		restConfig *rest.Config
		err        error
	)

	if cfg.KubeconfigPath != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags(cfg.ApiserverURL, cfg.KubeconfigPath)
	} else {
		restConfig, err = rest.InClusterConfig()
		if err == nil && cfg.ApiserverURL != "" {
			restConfig.Host = cfg.ApiserverURL
		}
	}

	if err != nil {
		klog.Fatalf("Failed to build kubeconfig: %v", err)
	}

	if cfg.ApiserverURL != "" {
		klog.Infof("Using API server URL override: %s", cfg.ApiserverURL)
	}

	// Wire client-go metrics into Prometheus before creating clients.
	metrics.RegisterClientGoMetrics()

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	webhookServer, err := webhook.NewServer(clientset, restConfig, "")
	if err != nil {
		klog.Fatalf("Failed to create webhook server: %v", err)
	}

	controllerNamespace := os.Getenv("POD_NAMESPACE")
	if controllerNamespace == "" {
		controllerNamespace = "default"
	}

	// Initialize the CertManager to obtain and rotate a cluster-CA-signed
	// serving certificate via the Kubernetes CSR API.
	certMgr := certmanager.NewCertManager(certmanager.Options{
		Clientset:   clientset,
		Namespace:   controllerNamespace,
		ServiceName: "unbounded-net-controller",
	})
	if err := certMgr.EnsureCertificate(ctx); err != nil {
		klog.Fatalf("Failed to ensure serving certificate: %v", err)
	}
	// Inject the CA bundle into webhook and APIService configurations so the
	// API server can verify the controller's self-signed serving certificate.
	if err := injectCABundle(ctx, clientset, restConfig, certMgr.CABundle(), controllerNamespace); err != nil {
		klog.Fatalf("Failed to inject CA bundle: %v", err)
	}

	go certMgr.RunRotationMonitor(ctx)

	// Create HMAC token issuer for node and viewer authentication.
	hmacKey := certMgr.HMACKey()
	if hmacKey == nil {
		klog.Fatalf("HMAC key not available from CertManager")
	}

	tokenIssuer, err := authn.NewTokenIssuer(hmacKey)
	if err != nil {
		klog.Fatalf("Failed to create token issuer: %v", err)
	}

	// Create health state tracker
	healthState := &healthState{
		clientset:                     clientset,
		nodeAgentHealthPort:           cfg.NodeAgentHealthPort,
		healthPort:                    cfg.HealthPort,
		azureTenantID:                 cfg.AzureTenantID,
		leaderElectionNS:              cfg.LeaderElection.ResourceNamespace,
		leaderElectionName:            cfg.LeaderElection.ResourceName,
		podIP:                         os.Getenv("POD_IP"),
		nodeName:                      os.Getenv("NODE_NAME"),
		statusCache:                   NewNodeStatusCache(),
		staleThreshold:                cfg.StatusStaleThreshold,
		tokenAuth:                     newTokenAuthenticator(clientset, []string{fmt.Sprintf("%s:unbounded-net-node", controllerNamespace)}),
		nodeServiceAccount:            fmt.Sprintf("%s:unbounded-net-node", controllerNamespace),
		registerAggregatedAPIServer:   cfg.RegisterAggregatedAPIServer,
		statusWSKeepaliveInterval:     cfg.StatusWSKeepaliveInterval,
		statusWSKeepaliveFailureCount: cfg.StatusWSKeepaliveFailureCount,
		nodeMTU:                       cfg.NodeMTU,
		maxPullConcurrency:            defaultMaxPullConcurrency,
		kubeProxyMonitor:              newKubeProxyMonitor(cfg.KubeProxyHealthInterval),
	}

	// Start kube-proxy health monitor in the background.
	go healthState.kubeProxyMonitor.Start(ctx)

	// Register all handlers on a unified mux and start the HTTPS server.
	startServer(ctx, cfg.HealthPort, cfg.RequireDashboardAuth, healthState, webhookServer, certMgr, tokenIssuer, tokenEndpointConfig{
		nodeTokenLifetime:   cfg.NodeTokenLifetime,
		viewerTokenLifetime: cfg.ViewerTokenLifetime,
	})

	if cfg.DryRun {
		klog.Warning("Dry-run mode is not supported for site-based pod CIDR assignment; running normally")
	}

	// runFunc creates and runs the controller - called only when becoming leader
	// This ensures the allocator and informer are created fresh with current state
	runFunc := func(ctx context.Context) {
		klog.Info("Creating informers and controllers")

		// Create informer factory
		informerFactory := informers.NewSharedInformerFactory(clientset, cfg.InformerResyncPeriod)

		// Create pod informer for unbounded-net-node pods (filtered by label selector)
		podInformerFactory := informers.NewSharedInformerFactoryWithOptions(clientset, cfg.InformerResyncPeriod,
			informers.WithNamespace(controllerNamespace),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = "app.kubernetes.io/name=unbounded-net-node"
			}),
		)
		podLister := podInformerFactory.Core().V1().Pods().Lister()

		var (
			gatewayPoolInformer cache.SharedIndexInformer
			sitePeeringInformer cache.SharedIndexInformer
			assignmentInformer  cache.SharedIndexInformer
			poolPeeringInformer cache.SharedIndexInformer
		)

		dynamicClient, err := dynamic.NewForConfig(restConfig)
		if err != nil {
			klog.Fatalf("Failed to create dynamic client: %v", err)
		}

		dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, cfg.InformerResyncPeriod)
		gatewayPoolInformer = dynamicInformerFactory.ForResource(gatewayPoolGVR).Informer()
		sitePeeringInformer = dynamicInformerFactory.ForResource(sitePeeringGVR).Informer()
		assignmentInformer = dynamicInformerFactory.ForResource(siteGatewayPoolAssignmentGVR).Informer()
		poolPeeringInformer = dynamicInformerFactory.ForResource(gatewayPoolPeeringGVR).Informer()

		// Create and start site controller (shares the node informer factory)
		siteCtrl, err := controller.NewSiteController(clientset, dynamicClient, dynamicInformerFactory, informerFactory)
		if err != nil {
			klog.Errorf("Failed to create site controller: %v", err)
		} else {
			// Wire the site controller as CIDR allocator for the mutating webhook
			webhookServer.SetCIDRAllocator(siteCtrl)

			// Set informers in health state for efficient lookups in status endpoints
			healthState.setInformers(siteCtrl.GetNodeLister(), podLister, siteCtrl.GetSiteInformer(), gatewayPoolInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer)

			healthState.siteController = siteCtrl
			if healthState.clusterStatusCache != nil {
				healthState.clusterStatusCache.MarkFullRebuildNeeded()
			}

			// Register a node event handler to log MTU mismatches when a
			// node is added, updated (e.g. annotation change), or resynced.
			if cfg.NodeMTU > 0 {
				nodeMTU := cfg.NodeMTU

				if _, err := informerFactory.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) {
						if node, ok := obj.(*corev1.Node); ok {
							checkNodeMTUAnnotation(nodeMTU, node)
						}
					},
					UpdateFunc: func(_, newObj interface{}) {
						if node, ok := newObj.(*corev1.Node); ok {
							checkNodeMTUAnnotation(nodeMTU, node)
						}
					},
				}); err != nil {
					klog.Warningf("Failed to register node MTU watcher: %v", err)
				}
			}

			// Mark cluster status dirty when informer-watched resources change
			// so the pre-built status is refreshed.
			markDirty := func(_ interface{}) {
				if healthState.clusterStatusCache != nil {
					healthState.clusterStatusCache.MarkFullRebuildNeeded()
				}
			}
			markDirtyUpdate := func(_, newObj interface{}) { markDirty(newObj) }
			dirtyHandler := cache.ResourceEventHandlerFuncs{
				AddFunc:    markDirty,
				UpdateFunc: markDirtyUpdate,
				DeleteFunc: markDirty,
			}

			for _, inf := range []cache.SharedIndexInformer{
				siteCtrl.GetSiteInformer(), gatewayPoolInformer,
				sitePeeringInformer, assignmentInformer, poolPeeringInformer,
			} {
				if inf != nil {
					if _, err := inf.AddEventHandler(dirtyHandler); err != nil {
						klog.Warningf("Failed to register cluster status dirty watcher: %v", err)
					}
				}
			}

			if _, err := informerFactory.Core().V1().Nodes().Informer().AddEventHandler(dirtyHandler); err != nil {
				klog.Warningf("Failed to register node dirty watcher for cluster status: %v", err)
			}

			// Trigger initial rebuild now that informers are set.
			if healthState.clusterStatusCache != nil {
				healthState.clusterStatusCache.MarkFullRebuildNeeded()
			}

			go func() {
				if err := siteCtrl.Run(ctx, 4); err != nil {
					klog.Errorf("Site controller error: %v", err)
				}
			}()
		}

		// Create and start gateway pool controller (shares the node informer factory)
		gatewayPoolCtrl, err := controller.NewGatewayPoolController(clientset, dynamicClient, dynamicInformerFactory, informerFactory)
		if err != nil {
			klog.Errorf("Failed to create gateway pool controller: %v", err)
		} else {
			go func() {
				if err := gatewayPoolCtrl.Run(ctx, 2); err != nil {
					klog.Errorf("Gateway pool controller error: %v", err)
				}
			}()
		}

		// Create and start peering aggregation controller
		peeringAggCtrl, err := controller.NewPeeringAggregationController(dynamicClient, dynamicInformerFactory)
		if err != nil {
			klog.Errorf("Failed to create peering aggregation controller: %v", err)
		} else {
			go func() {
				if err := peeringAggCtrl.Run(ctx, 2); err != nil {
					klog.Errorf("Peering aggregation controller error: %v", err)
				}
			}()
		}

		// Start informers after all informers are created.
		informerFactory.Start(ctx.Done())
		dynamicInformerFactory.Start(ctx.Done())
		podInformerFactory.Start(ctx.Done())
		podInformerFactory.WaitForCacheSync(ctx.Done())

		<-ctx.Done()
	}

	// Run with or without leader election
	if cfg.LeaderElection.Enabled {
		if forceNotLeader {
			klog.Info("Force-not-leader enabled; staying in standby mode and skipping leader election acquisition")
			healthState.setLeader(false)
			<-ctx.Done()

			return nil
		}

		klog.Info("Leader election enabled, waiting for leadership...")
		runLeaderElection(ctx, cfg, clientset, healthState, runFunc)
	} else {
		klog.Info("Leader election disabled")
		healthState.setLeader(true)
		runFunc(ctx)
	}

	return nil
}

// injectCABundle updates the webhook and APIService configurations with the
// controller's self-signed CA bundle so the API server can verify TLS
// connections to the webhook/aggregated API endpoints.
func injectCABundle(ctx context.Context, clientset kubernetes.Interface, restConfig *rest.Config, caBundle []byte, _ string) error {
	if len(caBundle) == 0 {
		return fmt.Errorf("CA bundle is empty")
	}

	// Update ValidatingWebhookConfiguration.
	vwc, err := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "unbounded-net-validating-webhook", metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to get validating webhook configuration (may not be deployed yet): %v", err)
	} else {
		for i := range vwc.Webhooks {
			vwc.Webhooks[i].ClientConfig.CABundle = caBundle
		}

		if _, err := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, vwc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update validating webhook caBundle: %w", err)
		}

		klog.Infof("Updated validating webhook configuration caBundle")
	}

	// Update MutatingWebhookConfiguration.
	mwc, err := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, "unbounded-net-mutating-webhook", metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to get mutating webhook configuration (may not be deployed yet): %v", err)
	} else {
		for i := range mwc.Webhooks {
			mwc.Webhooks[i].ClientConfig.CABundle = caBundle
		}

		if _, err := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, mwc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update mutating webhook caBundle: %w", err)
		}

		klog.Infof("Updated mutating webhook configuration caBundle")
	}

	// Update APIService.
	apiRegClient, err := apiregistrationclientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create apiregistration client: %w", err)
	}

	apiSvc, err := apiRegClient.ApiregistrationV1().APIServices().Get(ctx, "v1alpha1.status.net.unbounded-kube.io", metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to get APIService (may not be deployed yet): %v", err)
	} else {
		apiSvc.Spec.CABundle = caBundle
		if _, err := apiRegClient.ApiregistrationV1().APIServices().Update(ctx, apiSvc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update APIService caBundle: %w", err)
		}

		klog.Infof("Updated APIService caBundle")
	}

	return nil
}
